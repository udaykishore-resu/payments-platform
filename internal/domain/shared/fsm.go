package shared

import (
	"sort"
	"strings"

	"github.com/udaykishore-resu/payments-platform/pkg/apierror"
)

// StateMachine is a declarative transition table.
//
// Every finite state machine in this platform — merchant lifecycle, payment, payment attempt,
// refund, gateway health, gateway connection, workflow instance, workflow step, onboarding
// case, idempotency record, inbound webhook, reconciliation exception — is built from this
// type rather than from scattered `if current == X && next == Y` checks.
//
// Why a table and not conditionals:
//
//   - The set of legal transitions is *enumerable*, which means it can be tested exhaustively.
//     The property test in each aggregate iterates every (from, to) pair in the state universe
//     and asserts that exactly the pairs in the table are accepted. Conditional logic scattered
//     across methods cannot be tested that way, so the illegal transitions nobody thought of
//     stay untested until one of them happens in production.
//   - It is inspectable. The same table generates the documentation, the database CHECK
//     constraint and the diagram, so those three cannot drift.
//   - Adding a state is a localized change with a compile-time-visible blast radius.
//
// The type parameter is the state type, which is always a defined string type so that states
// are readable in the database and in logs.
type StateMachine[S ~string] struct {
	name        string
	transitions map[S]map[S]struct{}
	terminal    map[S]struct{}
	initial     S
	all         []S
}

// Transition declares one legal edge.
type Transition[S ~string] struct {
	From S
	To   S
}

// NewStateMachine builds a machine from its initial state, the complete state universe, the
// terminal states and the legal transitions.
//
// It panics if a transition references a state outside the declared universe. That is a
// programming error and it is caught by the package's own init-time test, never at runtime in
// a request path.
func NewStateMachine[S ~string](name string, initial S, all []S, terminal []S, transitions []Transition[S]) *StateMachine[S] {
	sm := &StateMachine[S]{
		name:        name,
		transitions: make(map[S]map[S]struct{}, len(all)),
		terminal:    make(map[S]struct{}, len(terminal)),
		initial:     initial,
		all:         append([]S(nil), all...),
	}
	universe := make(map[S]struct{}, len(all))
	for _, s := range all {
		universe[s] = struct{}{}
	}
	if _, ok := universe[initial]; !ok {
		panic("shared: state machine " + name + ": initial state not in universe")
	}
	for _, s := range terminal {
		if _, ok := universe[s]; !ok {
			panic("shared: state machine " + name + ": terminal state not in universe: " + string(s))
		}
		sm.terminal[s] = struct{}{}
	}
	for _, t := range transitions {
		if _, ok := universe[t.From]; !ok {
			panic("shared: state machine " + name + ": unknown from-state " + string(t.From))
		}
		if _, ok := universe[t.To]; !ok {
			panic("shared: state machine " + name + ": unknown to-state " + string(t.To))
		}
		if _, ok := sm.terminal[t.From]; ok {
			panic("shared: state machine " + name + ": terminal state " + string(t.From) + " has an outgoing transition")
		}
		if sm.transitions[t.From] == nil {
			sm.transitions[t.From] = make(map[S]struct{}, 4)
		}
		sm.transitions[t.From][t.To] = struct{}{}
	}
	sort.Slice(sm.all, func(i, j int) bool { return sm.all[i] < sm.all[j] })
	return sm
}

// Name returns the machine's name, used in error messages and metrics.
func (sm *StateMachine[S]) Name() string { return sm.name }

// Initial returns the state a new aggregate starts in.
func (sm *StateMachine[S]) Initial() S { return sm.initial }

// States returns the full state universe, sorted.
func (sm *StateMachine[S]) States() []S { return append([]S(nil), sm.all...) }

// IsKnown reports whether s is a declared state. Used when hydrating an aggregate from the
// database: a row carrying a state this binary does not know about means a rollback landed on
// data written by a newer version, and that must fail loudly rather than be coerced.
func (sm *StateMachine[S]) IsKnown(s S) bool {
	for _, x := range sm.all {
		if x == s {
			return true
		}
	}
	return false
}

// IsTerminal reports whether s has no outgoing transitions.
func (sm *StateMachine[S]) IsTerminal(s S) bool {
	_, ok := sm.terminal[s]
	return ok
}

// CanTransition reports whether from → to is legal.
func (sm *StateMachine[S]) CanTransition(from, to S) bool {
	if from == to {
		// Self-transitions are never implicitly legal. A state machine that silently allows
		// X → X hides idempotency bugs: the second capture of an already-captured payment
		// looks like it succeeded.
		_, explicit := sm.transitions[from][to]
		return explicit
	}
	_, ok := sm.transitions[from][to]
	return ok
}

// Next returns the sorted set of states reachable from s in one step.
func (sm *StateMachine[S]) Next(from S) []S {
	out := make([]S, 0, len(sm.transitions[from]))
	for s := range sm.transitions[from] {
		out = append(out, s)
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}

// Transition validates from → to, returning a typed INVALID_STATE_TRANSITION error listing
// the legal alternatives. The message is deliberately actionable: a client that gets
// "cannot capture a payment in state REFUNDED; from REFUNDED the payment may only move to
// DISPUTED" can fix its logic without opening a support ticket.
func (sm *StateMachine[S]) Transition(from, to S) error {
	if !sm.IsKnown(from) {
		return apierror.Newf(apierror.CodeInternalError,
			"%s: unknown current state %q — this row may have been written by a newer version of the service", sm.name, from)
	}
	if !sm.IsKnown(to) {
		return apierror.Newf(apierror.CodeInternalError, "%s: unknown target state %q", sm.name, to)
	}
	if sm.CanTransition(from, to) {
		return nil
	}
	if sm.IsTerminal(from) {
		return apierror.Newf(apierror.CodeInvalidStateTransition,
			"%s is in terminal state %s and cannot transition to %s", sm.name, from, to).
			WithDetail(apierror.Detail{
				Field:   "status",
				Code:    "TERMINAL_STATE",
				Message: string(from) + " is a terminal state",
				RuleID:  "L7." + strings.ToUpper(sm.name) + "_TRANSITION",
			})
	}
	allowed := sm.Next(from)
	strs := make([]string, len(allowed))
	for i, a := range allowed {
		strs[i] = string(a)
	}
	permitted := "none"
	if len(strs) > 0 {
		permitted = strings.Join(strs, ", ")
	}
	return apierror.Newf(apierror.CodeInvalidStateTransition,
		"%s cannot transition from %s to %s; permitted next states are: %s", sm.name, from, to, permitted).
		WithDetail(apierror.Detail{
			Field:   "status",
			Code:    "ILLEGAL_TRANSITION",
			Message: "from " + string(from) + " the permitted next states are: " + permitted,
			RuleID:  "L7." + strings.ToUpper(sm.name) + "_TRANSITION",
		})
}

// Edges returns every legal transition, sorted, for documentation generation, for the SQL
// CHECK-constraint generator, and for the exhaustive property test.
func (sm *StateMachine[S]) Edges() []Transition[S] {
	out := make([]Transition[S], 0, len(sm.transitions)*2)
	for from, tos := range sm.transitions {
		for to := range tos {
			out = append(out, Transition[S]{From: from, To: to})
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].From != out[j].From {
			return out[i].From < out[j].From
		}
		return out[i].To < out[j].To
	})
	return out
}

// Mermaid renders the machine as a mermaid stateDiagram-v2 block. Used by the documentation
// generator so the diagrams in docs/ are derived from the same table the code enforces and
// cannot drift from it.
func (sm *StateMachine[S]) Mermaid() string {
	var b strings.Builder
	b.WriteString("stateDiagram-v2\n")
	b.WriteString("    [*] --> " + string(sm.initial) + "\n")
	for _, e := range sm.Edges() {
		b.WriteString("    " + string(e.From) + " --> " + string(e.To) + "\n")
	}
	for _, s := range sm.all {
		if sm.IsTerminal(s) {
			b.WriteString("    " + string(s) + " --> [*]\n")
		}
	}
	return b.String()
}
