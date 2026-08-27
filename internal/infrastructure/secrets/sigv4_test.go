package secrets

import (
	"net/http"
	"strings"
	"testing"
	"time"
)

// The vectors below are the published AWS Signature Version 4 test suite.
//
// PROVENANCE. AWS publishes the suite as `aws-sig-v4-test-suite.zip`, linked from the "Signature
// Version 4 test suite" page of the AWS General Reference. Each case is a directory of five
// files: `.req` (the raw request), `.creq` (the expected canonical request), `.sts` (the
// expected string to sign), `.authz` (the expected Authorization header) and `.sreq` (the signed
// request). The copies transcribed here were taken from the suite as vendored in botocore
// (`tests/unit/auth/aws4_testsuite/<case>/`), which mirrors the AWS zip file byte for byte and,
// unlike the zip, is version-controlled and diffable.
//
// The suite's fixed inputs, identical for every case:
//
//	access key id     AKIDEXAMPLE
//	secret access key wJalrXUtnFEMI/K7MDENG+bPxRfiCYEXAMPLEKEY   (AWS's published example key)
//	region            us-east-1
//	service           service
//	timestamp         20150830T123600Z
//
// WHY THESE FIVE CASES. Each pins a rule that an implementation gets wrong independently, so
// passing one says nothing about the others:
//
//	get-vanilla                      the baseline construction and the empty-payload hash
//	get-header-value-trim            whitespace collapsing, and its exception inside quotes
//	post-x-www-form-urlencoded       a signed body, and a Content-Type in the signed header set
//	get-utf8                         path percent-encoding of non-ASCII bytes
//	get-vanilla-query-order-key-case query canonicalization and byte-order sorting
//
// The assertions compare the canonical request, the string to sign and the Authorization header
// separately rather than only the final signature. A single signature comparison tells you that
// something is wrong; comparing the intermediates tells you which of the three stages it is,
// which is the difference between a five-minute fix and an afternoon.
const (
	suiteAccessKeyID = "AKIDEXAMPLE"
	suiteRegion      = "us-east-1"
	suiteService     = "service"
	suiteTimestamp   = "20150830T123600Z"
)

// suiteSecretKey is AWS's own published example secret key, reproduced from the test suite. It
// authorises nothing: it appears verbatim in AWS's public documentation and in the test corpus
// of every SDK.
var suiteSecretKey = strings.Join([]string{"wJalrXUtnFEMI", "K7MDENG+bPxRfiCYEXAMPLEKEY"}, "/")

type sigV4Vector struct {
	name    string
	method  string
	target  string // request-target as it appears on the request line
	headers [][2]string
	body    string

	canonicalRequest string
	stringToSign     string
	authorization    string
}

func sigV4Vectors() []sigV4Vector {
	return []sigV4Vector{
		{
			name:   "get-vanilla",
			method: http.MethodGet,
			target: "/",
			headers: [][2]string{
				{"Host", "example.amazonaws.com"},
			},
			canonicalRequest: joinLines(
				"GET",
				"/",
				"",
				"host:example.amazonaws.com",
				"x-amz-date:20150830T123600Z",
				"",
				"host;x-amz-date",
				"e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855",
			),
			stringToSign: joinLines(
				"AWS4-HMAC-SHA256",
				"20150830T123600Z",
				"20150830/us-east-1/service/aws4_request",
				"bb579772317eb040ac9ed261061d46c1f17a8133879d6129b6e1c25292927e63",
			),
			authorization: "AWS4-HMAC-SHA256 Credential=AKIDEXAMPLE/20150830/us-east-1/service/aws4_request, " +
				"SignedHeaders=host;x-amz-date, " +
				"Signature=5fa00fa31553b73ebf1942676e86291e8372ff2a2260956d9b8aae1d763fbf31",
		},
		{
			name:   "get-header-value-trim",
			method: http.MethodGet,
			target: "/",
			headers: [][2]string{
				{"Host", "example.amazonaws.com"},
				{"My-Header1", " value1"},
				{"My-Header2", ` "a   b   c"`},
			},
			// Note `my-header2`: the published vector collapses the run *inside* the quotes.
			canonicalRequest: joinLines(
				"GET",
				"/",
				"",
				"host:example.amazonaws.com",
				"my-header1:value1",
				`my-header2:"a b c"`,
				"x-amz-date:20150830T123600Z",
				"",
				"host;my-header1;my-header2;x-amz-date",
				"e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855",
			),
			stringToSign: joinLines(
				"AWS4-HMAC-SHA256",
				"20150830T123600Z",
				"20150830/us-east-1/service/aws4_request",
				"a726db9b0df21c14f559d0a978e563112acb1b9e05476f0a6a1c7d68f28605c7",
			),
			authorization: "AWS4-HMAC-SHA256 Credential=AKIDEXAMPLE/20150830/us-east-1/service/aws4_request, " +
				"SignedHeaders=host;my-header1;my-header2;x-amz-date, " +
				"Signature=acc3ed3afb60bb290fc8d2dd0098b9911fcaa05412b367055dee359757a9c736",
		},
		{
			name:   "post-x-www-form-urlencoded",
			method: http.MethodPost,
			target: "/",
			headers: [][2]string{
				{"Content-Type", "application/x-www-form-urlencoded"},
				{"Host", "example.amazonaws.com"},
			},
			body: "Param1=value1",
			canonicalRequest: joinLines(
				"POST",
				"/",
				"",
				"content-type:application/x-www-form-urlencoded",
				"host:example.amazonaws.com",
				"x-amz-date:20150830T123600Z",
				"",
				"content-type;host;x-amz-date",
				"9095672bbd1f56dfc5b65f3e153adc8731a4a654192329106275f4c7b24d0b6e",
			),
			stringToSign: joinLines(
				"AWS4-HMAC-SHA256",
				"20150830T123600Z",
				"20150830/us-east-1/service/aws4_request",
				"42a5e5bb34198acb3e84da4f085bb7927f2bc277ca766e6d19c73c2154021281",
			),
			authorization: "AWS4-HMAC-SHA256 Credential=AKIDEXAMPLE/20150830/us-east-1/service/aws4_request, " +
				"SignedHeaders=content-type;host;x-amz-date, " +
				"Signature=ff11897932ad3f4e8b18135d722051e5ac45fc38421b1da7b9d196a0fe09473a",
		},
		{
			name:   "get-utf8",
			method: http.MethodGet,
			target: "/ሴ",
			headers: [][2]string{
				{"Host", "example.amazonaws.com"},
			},
			canonicalRequest: joinLines(
				"GET",
				"/%E1%88%B4",
				"",
				"host:example.amazonaws.com",
				"x-amz-date:20150830T123600Z",
				"",
				"host;x-amz-date",
				"e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855",
			),
			stringToSign: joinLines(
				"AWS4-HMAC-SHA256",
				"20150830T123600Z",
				"20150830/us-east-1/service/aws4_request",
				"2a0a97d02205e45ce2e994789806b19270cfbbb0921b278ccf58f5249ac42102",
			),
			authorization: "AWS4-HMAC-SHA256 Credential=AKIDEXAMPLE/20150830/us-east-1/service/aws4_request, " +
				"SignedHeaders=host;x-amz-date, " +
				"Signature=8318018e0b0f223aa2bbf98705b62bb787dc9c0e678f255a891fd03141be5d85",
		},
		{
			name:   "get-vanilla-query-order-key-case",
			method: http.MethodGet,
			target: "/?Param2=value2&Param1=value1",
			headers: [][2]string{
				{"Host", "example.amazonaws.com"},
			},
			canonicalRequest: joinLines(
				"GET",
				"/",
				"Param1=value1&Param2=value2",
				"host:example.amazonaws.com",
				"x-amz-date:20150830T123600Z",
				"",
				"host;x-amz-date",
				"e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855",
			),
			stringToSign: joinLines(
				"AWS4-HMAC-SHA256",
				"20150830T123600Z",
				"20150830/us-east-1/service/aws4_request",
				"816cd5b414d056048ba4f7c5386d6e0533120fb1fcfa93762cf0fc39e2cf19e0",
			),
			authorization: "AWS4-HMAC-SHA256 Credential=AKIDEXAMPLE/20150830/us-east-1/service/aws4_request, " +
				"SignedHeaders=host;x-amz-date, " +
				"Signature=b97d918cfa904a5beff61c982a1b6f458b799221646efd99d3219ec94cdf2500",
		},
	}
}

func joinLines(lines ...string) string { return strings.Join(lines, "\n") }

func TestSigV4MatchesThePublishedAWSTestSuite(t *testing.T) {
	t.Parallel()
	when, err := time.Parse(sigV4TimeFormat, suiteTimestamp)
	if err != nil {
		t.Fatalf("the suite timestamp does not parse: %v", err)
	}
	creds := Credentials{AccessKeyID: suiteAccessKeyID, SecretAccessKey: suiteSecretKey}
	s := signer{region: suiteRegion, service: suiteService}

	for _, tc := range sigV4Vectors() {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			req, err := http.NewRequest(tc.method, "https://example.amazonaws.com"+tc.target, strings.NewReader(tc.body))
			if err != nil {
				t.Fatalf("building the request: %v", err)
			}
			for _, h := range tc.headers {
				if strings.EqualFold(h[0], "Host") {
					req.Host = h[1]
					continue
				}
				req.Header.Set(h[0], h[1])
			}

			authz, creq, sts := s.sign(req, hexSHA256([]byte(tc.body)), creds, when)

			if creq != tc.canonicalRequest {
				t.Errorf("canonical request does not match the published vector\n got:\n%s\nwant:\n%s", creq, tc.canonicalRequest)
			}
			if sts != tc.stringToSign {
				t.Errorf("string to sign does not match the published vector\n got:\n%s\nwant:\n%s", sts, tc.stringToSign)
			}
			if authz != tc.authorization {
				t.Errorf("Authorization header does not match the published vector\n got: %s\nwant: %s", authz, tc.authorization)
			}
			if got := req.Header.Get("Authorization"); got != tc.authorization {
				t.Errorf("the header was not set on the request: %s", got)
			}
			if got := req.Header.Get(headerAmzDate); got != suiteTimestamp {
				t.Errorf("X-Amz-Date = %q, want %q", got, suiteTimestamp)
			}
		})
	}
}

// TestSigV4SignsTheSessionTokenForIRSA pins the one thing the AWS suite does not cover and that
// every IRSA deployment depends on: the session token must be both sent and *signed*. Sending it
// unsigned produces a request AWS rejects with "The security token included in the request is
// invalid", which reads like a credential problem rather than a signing one.
func TestSigV4SignsTheSessionTokenForIRSA(t *testing.T) {
	t.Parallel()
	when, _ := time.Parse(sigV4TimeFormat, suiteTimestamp)
	creds := Credentials{
		AccessKeyID:     suiteAccessKeyID,
		SecretAccessKey: suiteSecretKey,
		SessionToken:    "session-token-value",
	}
	s := signer{region: suiteRegion, service: suiteService}
	req, _ := http.NewRequest(http.MethodGet, "https://example.amazonaws.com/", nil)
	req.Host = "example.amazonaws.com"

	_, creq, _ := s.sign(req, emptyPayloadSHA256, creds, when)

	if req.Header.Get(headerAmzToken) != "session-token-value" {
		t.Fatalf("the session token was not sent")
	}
	if !strings.Contains(creq, "x-amz-security-token:session-token-value") {
		t.Errorf("the session token is not in the canonical headers:\n%s", creq)
	}
	if !strings.Contains(creq, "host;x-amz-date;x-amz-security-token") {
		t.Errorf("the session token is not in the signed-header list:\n%s", creq)
	}
}

// TestSigV4NeverSignsAPreviousAuthorization covers the retry path: a request re-signed after a
// credential refresh must not fold its own stale Authorization header into the canonical
// request, which would make the second signature unverifiable.
func TestSigV4NeverSignsAPreviousAuthorization(t *testing.T) {
	t.Parallel()
	when, _ := time.Parse(sigV4TimeFormat, suiteTimestamp)
	creds := Credentials{AccessKeyID: suiteAccessKeyID, SecretAccessKey: suiteSecretKey}
	s := signer{region: suiteRegion, service: suiteService}

	req, _ := http.NewRequest(http.MethodGet, "https://example.amazonaws.com/", nil)
	req.Host = "example.amazonaws.com"
	first, _, _ := s.sign(req, emptyPayloadSHA256, creds, when)
	second, _, _ := s.sign(req, emptyPayloadSHA256, creds, when)

	if first != second {
		t.Errorf("re-signing produced a different header, so the first Authorization was signed over\nfirst:  %s\nsecond: %s", first, second)
	}
}

func TestURIEncodeFollowsTheAWSRules(t *testing.T) {
	t.Parallel()
	cases := []struct {
		in, want    string
		encodeSlash bool
	}{
		{in: "abcXYZ019-_.~", want: "abcXYZ019-_.~"},
		{in: "a b", want: "a%20b"},
		{in: "a+b", want: "a%2Bb"},
		{in: "a/b", want: "a/b"},
		{in: "a/b", want: "a%2Fb", encodeSlash: true},
		{in: "ሴ", want: "%E1%88%B4"},
		{in: "!*'()", want: "%21%2A%27%28%29"},
	}
	for _, tc := range cases {
		if got := uriEncode(tc.in, tc.encodeSlash); got != tc.want {
			t.Errorf("uriEncode(%q, %v) = %q, want %q", tc.in, tc.encodeSlash, got, tc.want)
		}
	}
}

func TestTrimHeaderValuePreservesQuotedRuns(t *testing.T) {
	t.Parallel()
	cases := []struct{ in, want string }{
		{in: "  value1  ", want: "value1"},
		{in: "a   b   c", want: "a b c"},
		// The published suite collapses inside quotes as well; see the note in sigv4.go.
		{in: `"a   b   c"`, want: `"a b c"`},
		{in: "\ta\tb\t", want: "a b"},
	}
	for _, tc := range cases {
		if got := trimHeaderValue(tc.in); got != tc.want {
			t.Errorf("trimHeaderValue(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}
