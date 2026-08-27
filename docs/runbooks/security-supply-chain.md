# RB-032: Image / supply-chain compromise

> **First action: freeze deploys, pin production to the last known-good digest, SBOM diff.** In that
> order. Freezing first stops the compromised artefact spreading while you work out what it is.

- **Severity:** page (Sev-1)
- **Alert:** no dedicated Prometheus rule. Reached from §9.1 detections: an **admission denial**
  (unsigned or unprovenanced image — page), a **Falco shell-in-container** event (page, Sev-1), an
  egress-proxy denied destination (page security), a `govulncheck` or scanner finding on a running
  image, or an upstream advisory. One of the five incident classes in `docs/security.md` §9.3.
- **Triggered when:** an artefact running in production is believed to be, or to contain, something
  it should not.
- **Plane / service:** all planes · the build and deploy path
- **Related:** `docs/security.md` §8 (supply chain), §9.1, §9.3, `docs/deployment.md`,
  [security-credential-rotation.md](security-credential-rotation.md)

## What this means

The supply-chain controls are preventive and they run at admission: images are signed, carry
provenance, and are pinned by **digest** rather than by tag. An admission denial means one of those
properties was missing — which is either a pipeline defect or an attempt to run something the
pipeline did not build.

A shell in a container is a different signal entirely: the pods run with a read-only root filesystem,
dropped capabilities and no shell in the runtime image by design. Falco firing on one means either
the image is not what it should be, or someone got in.

The three questions, in order: **what is running, where did it come from, and what did it touch.**

## Impact

- **Unknown until scoped, and that is the problem.** A compromised image in the payment path could
  read credentials at point of use, alter payment behaviour, or exfiltrate.
- **Deploys are frozen**, so no other fix can ship until this is resolved. Plan for that.
- **Every credential the compromised workload could resolve is potentially exposed** — which is why
  [security-credential-rotation.md](security-credential-rotation.md) usually runs in parallel.
- If it is an admission denial with nothing actually running, impact is zero and the finding is the
  pipeline. Establish which case you are in early.

## Immediate triage (first 5 minutes)

1. **Freeze deploys.** Suspend the CD sync (`deployments/argocd/`) so nothing else rolls out. This
   is the first action and it takes seconds.
2. **What is actually running, by digest:**
   ```bash
   kubectl get pods -A -o jsonpath=\
   '{range .items[*]}{.metadata.namespace}{"\t"}{.metadata.name}{"\t"}{range .status.containerStatuses[*]}{.imageID}{" "}{end}{"\n"}{end}' \
     | sort -u
   ```
   `imageID` is the digest actually pulled. A tag can be re-pointed; a digest cannot.
3. **Pin production to the last known-good digest:**
   ```bash
   kubectl -n pp-data-plane set image deployment/payment-api \
     payment-api=ghcr.io/udaykishore-resu/payments-platform/payment-api@sha256:<known-good>
   kubectl -n pp-data-plane rollout status deployment/payment-api --timeout=5m
   ```
   Repeat per affected deployment. Pin by digest, never by tag, for exactly the reason above.
4. **SBOM diff.** Generate one for the current source and compare against the release SBOM for the
   suspect image:
   ```bash
   make sbom          # writes sbom/source.cyclonedx.json when syft is installed
   ```
   The multi-arch build attaches provenance and an SBOM (`--provenance=true --sbom=true`), so the
   released artefact's SBOM is retrievable and comparable. The diff names the added or changed
   component, which is usually the whole answer.
5. **Vulnerability check against the linked dependency graph:**
   ```bash
   make vuln          # govulncheck ./...
   ./scripts/check-licences.sh
   ```
6. **Admission and runtime evidence:**
   ```bash
   kubectl get events -A --sort-by=.lastTimestamp | grep -iE 'admission|denied|policy' | tail -30
   ```
   Plus the Falco alert and the memory snapshot: §9.1's automatic mitigation for shell-in-container
   is *pod cordoned and evicted with a memory snapshot preserved*. That snapshot is the forensics.
7. **Page security.**

## Diagnosis

- **Admission denial, nothing running from the denied image** → the control worked. This is a
  pipeline or a policy finding, not a compromise. Treat as T-13 until cleared, then close. → *M4*.
- **Admission denial, but an earlier build of the same artefact is running** → establish whether
  that one was signed and provenanced. → *M1*.
- **Falco: shell in a container** → the pod is cordoned and evicted automatically with a memory
  snapshot preserved. Forensics on the snapshot. → *M2*, and assume credential exposure.
- **A dependency has a published advisory and is in the linked graph** → `govulncheck` reports
  reachability, which is what distinguishes "present" from "exploitable". → *M3*.
- **SBOM diff shows an unexpected component** → the build pulled something the source does not
  declare. → *M1*, and the build path is the finding.
- **Egress proxy denied an unexpected destination from a workload** → possible exfiltration attempt,
  or a dependency phoning home. §9.1 pages security on any occurrence. → *M2*.
- **An image digest is running that no pipeline record accounts for** → someone deployed outside the
  pipeline. That is the incident.
- **A base-image advisory with no evidence of exploitation** → patch on the normal cadence, not as
  an incident. Say so, and unfreeze.

## Mitigation

**M1 — pin to the last known-good digest** (step 3 above) across every affected deployment. Expected:
the compromised artefact stops running within one rollout. Verify by digest, not by tag.

**M2 — treat credentials as exposed.** Any credential the workload could resolve at point of use is
in scope: [security-credential-rotation.md](security-credential-rotation.md). Rotate, denylist the
token family, and **take the audit snapshot before deleting the old credential**.

**M3 — patch and rebuild** through the normal pipeline. The pipeline is the control; bypassing it to
ship a fix faster reproduces the problem you are fixing. `go.mod` is a shared, reviewed artefact —
a dependency bump is a reviewed change, and no build target mutates it.

**M4 — fix the pipeline.** If admission denied a legitimate image, signing or provenance is broken.
That is a build-path defect, and it is more urgent than it looks: a broken signing step means the
next real compromise has nothing to deny.

**M5 — preserve forensics before cleanup.** The memory snapshot from the evicted pod, the image
digests, the admission events, the egress-proxy denials, the SBOM diff. Store them in the incident
evidence location before anything is deleted or re-rolled.

**M6 — verify the whole estate**, not just the implicated deployment:
```bash
kubectl get pods -A -o jsonpath='{range .items[*]}{range .status.containerStatuses[*]}{.imageID}{"\n"}{end}{end}' \
  | sort -u
```
Compare every digest against the release record. One unexplained digest anywhere is the incident
continuing.

## Rollback / escalation

- **Deploys stay frozen** until the source of the artefact is established. Announce it explicitly —
  teams will otherwise assume CI is broken and start looking for workarounds.
- **Never bypass admission control to ship a fix.** The signature and provenance requirements are
  what make the pinned digest meaningful; an exception for the fix is an exception for anything.
- **Never `docker pull` and run a suspect image locally to inspect it.** Analyse it in an isolated
  environment or from its SBOM and manifest.
- **Shell in a container, or confirmed unexpected code execution** → Sev-1, security-led, and the
  scope includes every credential and every store the workload could reach.
- **If the compromise reached the build system itself**, the scope is every artefact built since,
  and that is a much larger incident with its own escalation.
- **If a gateway credential is in scope**, money movement is in scope:
  [reconciliation.md](reconciliation.md) and the payments product owner.

## Verification

```bash
# Every running digest is accounted for in the release record.
kubectl get pods -A -o jsonpath='{range .items[*]}{range .status.containerStatuses[*]}{.imageID}{"\n"}{end}{end}' | sort -u

make vuln                       # govulncheck clean, or findings triaged and recorded
./scripts/check-licences.sh     # dependency graph clean
./scripts/check-secrets.sh      # no material anywhere it should not be
make sbom                       # regenerated and compared
```
```bash
kubectl get events -A --sort-by=.lastTimestamp | grep -iE 'admission|denied' | tail
```
No new admission denials. Then confirm the platform is behaving: build, vet and the full test suite
against the pinned commit —
```bash
go build ./... && go vet ./... && go test ./... -race -count=1
```
— and payments flowing at baseline. Unfreeze deploys deliberately, by a named person, once the
source is established and the pipeline controls are verified.

## Follow-up

- The provenance question: **where did the artefact come from, and what allowed it to run?** Answer
  both; either alone leaves the hole.
- If signing or provenance was broken, the fix is the build path, and the test is an intentional
  negative — an unsigned image must be denied, and that should be exercised, not assumed.
- If a dependency was the vector, review how dependencies enter: `go.mod` is pinned and reviewed,
  `scripts/check-licences.sh` gates the graph, and `make vuln` checks reachability. Which of the
  three should have caught it is the finding.
- Retain the SBOM diff and the digest inventory as evidence.
- If deploys were frozen for a long period, note what shipped late. A freeze that blocks a security
  patch is a tension worth naming in the postmortem rather than discovering next time.
