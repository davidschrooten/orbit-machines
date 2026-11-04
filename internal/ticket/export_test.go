package ticket

import (
	"encoding/base64"
	"encoding/json"
)

// SignRawForTest signs arbitrary payload bytes so tests can exercise malformed JSON
// paths after valid HMAC verification.
func (c *Codec) SignRawForTest(rawPayload []byte) string {
	return c.signPayloadBytes(rawPayload)
}

// SignCustomSegmentForTest signs an arbitrary middle segment string so tests can
// exercise invalid base64 payload decoding after valid HMAC verification.
func (c *Codec) SignCustomSegmentForTest(segment string) string {
	signingInput := VersionPrefix + "." + segment
	return signingInput + "." + base64.RawURLEncoding.EncodeToString(c.computeMAC(signingInput))
}

// SignUncheckedForTest signs a Claims struct without static pre-validation so tests
// can exercise temporal verification errors (e.g. future iat).
func (c *Codec) SignUncheckedForTest(claims Claims) string {
	b, _ := json.Marshal(claims)
	return c.signPayloadBytes(b)
}
