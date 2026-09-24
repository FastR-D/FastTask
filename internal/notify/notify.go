// Package notify carries one outbound notification to the outside world.
//
// It is the generic `Notifier` Port doc/tech.md §2.3 reserved for this: an
// application service hands a Message and an address to a Sender, and the Sender
// is the only thing that knows a provider's wire format. Adding a provider means
// adding one file here and one case in NewSender — no application, HTTP or
// storage change, because a channel's provider-specific configuration travels as
// opaque settings plus one secret.
//
// Two rules shape every adapter in this package:
//
//   - A provider credential never appears in an error, a log line or a URL that
//     leaves the process (doc/tech.md §12). Errors carry the provider's own
//     diagnosis, never the request that produced it.
//   - A rejected address is distinguished from a failed call. ErrInvalidTarget
//     means the address itself is permanently unusable — an unregistered device
//     token, a chat that blocked the bot — so the caller stops retrying and
//     disables the target. Everything else is retryable or a configuration fault.
package notify

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"net"
	"net/http"
	"sort"
	"strings"
	"time"
)

// The supported providers. These strings are the `provider` column of
// notification_channels and the value the admin UI's provider select submits, so
// they are part of the stored contract, not an internal detail.
const (
	KindTelegram = "telegram"
	KindBark     = "bark"
	KindFCM      = "fcm"
	KindAPNS     = "apns"
)

// DefaultTimeout bounds one provider call. Push and chat APIs answer in well
// under a second; this is the ceiling for a slow one, not an expectation.
const DefaultTimeout = 10 * time.Second

// ErrInvalidTarget reports an address the provider permanently rejects. The
// dispatcher disables the target instead of retrying it forever (doc/tech.md
// §11.3: an input fault is not retryable).
var ErrInvalidTarget = errors.New("notify: the provider permanently rejected this address")

// ErrUndeliverable reports a message that cannot be delivered for a reason that is
// not the address: the channel's own credentials were refused, or the payload is
// one the provider will never accept. Retrying is as useless as it is for a dead
// address, but the target stays active — the fault belongs to the channel, and an
// administrator fixing it should find the subscriptions intact.
var ErrUndeliverable = errors.New("notify: the provider will not accept this message")

// Message is one notification. It is deliberately provider-neutral: each adapter
// decides how much of it its protocol can carry, and drops the rest rather than
// inventing a lowest-common-denominator payload.
type Message struct {
	// Topic is the stable event name ("proposal.pending", "notification.test").
	// Providers with a grouping concept (Bark group, FCM collapse key) use it.
	Topic string
	Title string
	Body  string
	// URL is an optional deep link back into FastTask.
	URL string
	// CollapseID lets a newer message replace an undelivered older one.
	CollapseID string
	// Data carries small string metadata. FCM delivers it as the data payload,
	// APNs as custom keys next to `aps`.
	Data map[string]string
}

// text is the plain-text rendering used by providers that carry one string
// (Telegram, and Bark's body when it has no title slot). Markdown is not used: a
// notification that fails to render is worse than one that is plain, and titles
// here come from user-authored task text.
func (m Message) text() string {
	parts := make([]string, 0, 3)
	if title := strings.TrimSpace(m.Title); title != "" {
		parts = append(parts, title)
	}
	if body := strings.TrimSpace(m.Body); body != "" {
		parts = append(parts, body)
	}
	if link := strings.TrimSpace(m.URL); link != "" {
		parts = append(parts, link)
	}
	return strings.Join(parts, "\n")
}

// Receipt is what the provider returned for a delivered message. ProviderID is
// the provider's own message id, kept so a delivery can be traced on their side.
type Receipt struct {
	ProviderID string
}

// Sender delivers to one address of one provider and can prove its own
// credentials without disturbing a user.
type Sender interface {
	// Kind is the provider this sender speaks (KindTelegram, ...).
	Kind() string
	// Verify checks the channel's credentials against the provider. It must not
	// deliver anything to a real user's device.
	Verify(ctx context.Context) error
	// Send delivers msg to address (a chat id, a Bark device key, an FCM
	// registration token, an APNs device token).
	Send(ctx context.Context, address string, msg Message) (Receipt, error)
}

// Spec is everything an adapter needs: the provider, the non-secret settings an
// administrator typed, and the decrypted secret. It is assembled from a
// notification_channels row, so a Spec never outlives one send or one verify.
type Spec struct {
	Kind     string
	Endpoint string
	Settings map[string]string
	Secret   string
	// Timeout bounds each provider call. Zero means DefaultTimeout.
	Timeout time.Duration
	// Client overrides the HTTP client. Tests use it to point an adapter at an
	// httptest server (and, for APNs, at an HTTP/2-capable TLS listener).
	Client *http.Client
}

// setting reads one non-secret setting, trimmed.
func (s Spec) setting(key string) string { return strings.TrimSpace(s.Settings[key]) }

// timeout is the per-call budget an adapter applies to its own context.
func (s Spec) timeout() time.Duration {
	if s.Timeout <= 0 {
		return DefaultTimeout
	}
	return s.Timeout
}

// client is the HTTP client an adapter uses: the injected one, or one built for
// this call and closed by the caller's context.
func (s Spec) client(timeout time.Duration) *http.Client {
	if s.Client != nil {
		return s.Client
	}
	return NewHTTPClient(timeout)
}

// Kinds lists the supported providers in stable order. The admin UI's provider
// list and the migration's CHECK constraint both follow it.
func Kinds() []string {
	kinds := []string{KindTelegram, KindBark, KindFCM, KindAPNS}
	sort.Strings(kinds)
	return kinds
}

// NewSender builds the adapter for spec.Kind. Construction performs every
// structural check the provider needs — a missing bot token, an unparsable .p8
// key, a malformed service-account JSON — and makes no network call, so it is
// also how the admin API validates a channel before it is stored.
func NewSender(spec Spec) (Sender, error) {
	switch strings.ToLower(strings.TrimSpace(spec.Kind)) {
	case KindTelegram:
		return newTelegram(spec)
	case KindBark:
		return newBark(spec)
	case KindFCM:
		return newFCM(spec)
	case KindAPNS:
		return newAPNS(spec)
	case "":
		return nil, errors.New("notify: a provider is required")
	default:
		return nil, fmt.Errorf("notify: unsupported provider %q", spec.Kind)
	}
}

// NewHTTPClient builds the client every adapter uses when none is injected.
// doc/tech.md §12 lists the transport settings this must carry; redirects are
// refused outright, because a credential in an Authorization header or a URL path
// must not be replayed to a host the operator did not configure.
func NewHTTPClient(timeout time.Duration) *http.Client {
	if timeout <= 0 {
		timeout = DefaultTimeout
	}
	return &http.Client{
		Timeout: timeout,
		Transport: &http.Transport{
			Proxy:                 http.ProxyFromEnvironment,
			DialContext:           (&net.Dialer{Timeout: 10 * time.Second, KeepAlive: 30 * time.Second}).DialContext,
			TLSHandshakeTimeout:   10 * time.Second,
			ResponseHeaderTimeout: timeout,
			IdleConnTimeout:       90 * time.Second,
			MaxIdleConns:          10,
			MaxIdleConnsPerHost:   5,
			ForceAttemptHTTP2:     true,
			TLSClientConfig:       &tls.Config{MinVersion: tls.VersionTLS12},
		},
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
}

// IsInvalidTarget reports whether err means "stop retrying this address".
func IsInvalidTarget(err error) bool { return errors.Is(err, ErrInvalidTarget) }

// IsUndeliverable reports whether err means "stop retrying this message".
func IsUndeliverable(err error) bool { return errors.Is(err, ErrUndeliverable) }

// addressShapes is the entry-time check for a target address. It is deliberately
// looser than each provider's own rule: the provider is the authority, and an address
// it rejects retires the target (ErrInvalidTarget). What this catches is a typo or a
// pasted notification URL, which would otherwise become a target that fails on every
// attempt and silently stops a user's notifications.
var addressShapes = map[string]func(string) bool{
	// A chat id is numeric (a supergroup id is negative) or a @username.
	KindTelegram: func(value string) bool {
		if strings.HasPrefix(value, "@") {
			return len(value) >= 5 && len(value) <= 36 && isUsernameBody(value[1:])
		}
		if strings.HasPrefix(value, "-") {
			value = value[1:]
		}
		return value != "" && len(value) <= 32 && isDigits(value)
	},
	// A Bark device key is the path segment of the push URL the app shows.
	KindBark: func(value string) bool {
		return len(value) >= 8 && len(value) <= 128 && isTokenBody(value)
	},
	// An FCM registration token is long, opaque and URL-safe.
	KindFCM: func(value string) bool {
		return len(value) >= 20 && len(value) <= 4096 && isTokenBody(value)
	},
	// An APNs device token is 32 bytes of hex as the app receives it.
	KindAPNS: func(value string) bool {
		if len(value) < 32 || len(value) > 512 || len(value)%2 != 0 {
			return false
		}
		for _, r := range value {
			if !strings.ContainsRune("0123456789abcdefABCDEF", r) {
				return false
			}
		}
		return true
	},
}

// ValidateAddress reports whether address is shaped like something kind can deliver
// to. An unknown kind is rejected: a channel's provider is fixed when it is created,
// so an address arriving for a provider this build does not know is a bug, not a typo.
func ValidateAddress(kind, address string) error {
	address = strings.TrimSpace(address)
	if address == "" {
		return errors.New("notify: an address is required")
	}
	shape, ok := addressShapes[strings.ToLower(strings.TrimSpace(kind))]
	if !ok {
		return fmt.Errorf("notify: unsupported provider %q", kind)
	}
	if !shape(address) {
		return fmt.Errorf("notify: %q is not a %s address", maskAddress(address), kind)
	}
	return nil
}

// maskAddress keeps a rejected address out of an error message. It is a device token
// or a chat id: an identifier, not something to echo into a log or an API response.
func maskAddress(address string) string {
	if len(address) <= 8 {
		return "the address"
	}
	return address[:4] + "..." + address[len(address)-4:]
}

func isDigits(value string) bool {
	for _, r := range value {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

func isUsernameBody(value string) bool {
	for _, r := range value {
		if !(r >= 'a' && r <= 'z') && !(r >= 'A' && r <= 'Z') && !(r >= '0' && r <= '9') && r != '_' {
			return false
		}
	}
	return true
}

func isTokenBody(value string) bool {
	for _, r := range value {
		if !(r >= 'a' && r <= 'z') && !(r >= 'A' && r <= 'Z') && !(r >= '0' && r <= '9') &&
			r != '_' && r != '-' && r != ':' && r != '.' && r != '%' {
			return false
		}
	}
	return true
}
