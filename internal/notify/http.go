package notify

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
)

// maxResponseBytes bounds a provider response. Push and chat APIs answer with a
// few hundred bytes; the limit exists so a misconfigured endpoint that happens to
// serve a large body cannot be read into memory unbounded (doc/tech.md §12).
const maxResponseBytes = 1 << 20

// answer is what one provider call produced: the status, the response headers an
// adapter may need (APNs returns its message id in apns-id), and the bounded body.
type answer struct {
	Status int
	Header http.Header
	Body   []byte
}

// exchange performs req and, when out is not nil, decodes a JSON body into it.
// A transport failure is reported as an error with a zero answer, which every
// adapter treats as retryable: nothing was accepted by the provider, so nothing was
// delivered.
func exchange(client *http.Client, req *http.Request, out any) (answer, error) {
	response, err := client.Do(req)
	if err != nil {
		return answer{}, err
	}
	defer response.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(response.Body, maxResponseBytes))
	if err != nil {
		return answer{Status: response.StatusCode, Header: response.Header}, err
	}
	result := answer{Status: response.StatusCode, Header: response.Header, Body: raw}
	if out != nil && len(bytes.TrimSpace(raw)) > 0 {
		// A provider that answers with a non-JSON body is still answering; the status
		// code decides the outcome, so a parse failure is dropped rather than failing a
		// delivery that may have succeeded.
		_ = json.Unmarshal(raw, out)
	}
	return result, nil
}

// providerError builds the error an adapter returns for a call the provider
// rejected. detail is the provider's own diagnosis, which is safe to surface to an
// administrator; the request that produced it is not, because it carries the
// credential.
func providerError(kind string, status int, detail string) error {
	detail = strings.TrimSpace(detail)
	if detail == "" {
		detail = http.StatusText(status)
	}
	return fmt.Errorf("%s: provider answered %d: %s", kind, status, detail)
}

// containsAny reports whether haystack contains one of the needles, case
// insensitively. Adapters use it to recognise the wording a provider uses for a
// dead address, which is documented as a message rather than as a code.
func containsAny(haystack string, needles ...string) bool {
	lowered := strings.ToLower(haystack)
	for _, needle := range needles {
		if strings.Contains(lowered, needle) {
			return true
		}
	}
	return false
}

// ErrNoAddress is returned when a target has no address at all. It wraps
// ErrInvalidTarget: there is nothing a retry could deliver to, so the caller
// disables the target exactly as it does for a rejected one.
var ErrNoAddress = fmt.Errorf("notify: the target has no address: %w", ErrInvalidTarget)
