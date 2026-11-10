package premiumizeme

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"strings"
	"testing"
)

type submissionErrorTransport func(*http.Request) (*http.Response, error)

func (f submissionErrorTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	return f(r)
}

func TestTransferSubmissionClassifiesOnlyDefinitePreSendErrors(t *testing.T) {
	for _, tt := range []struct {
		name   string
		err    error
		unsent bool
	}{
		{"dial", &net.OpError{Op: "dial", Net: "tcp", Err: errors.New("test-secret connection refused")}, true},
		{"read", &net.OpError{Op: "read", Net: "tcp", Err: io.EOF}, false},
		{"cancelled", context.Canceled, false},
		{"timeout", context.DeadlineExceeded, false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			pm := NewPremiumizemeClient("test-secret")
			pm.HTTPClient = &http.Client{Transport: submissionErrorTransport(func(*http.Request) (*http.Response, error) {
				return nil, tt.err
			})}
			_, err := pm.CreateTransferFromBytes(context.Background(), TransferSourceMagnet, []byte("magnet:?xt=urn:btih:123"), "", "folder")
			if err == nil || errors.Is(err, ErrTransferNotSubmitted) != tt.unsent {
				t.Fatalf("definitely unsubmitted = %v, want %v; error=%v", errors.Is(err, ErrTransferNotSubmitted), tt.unsent, err)
			}
			if strings.Contains(err.Error(), "test-secret") {
				t.Fatal("submission error exposed the API key")
			}
		})
	}
}

func TestTransferValidationFailuresAreNotSubmitted(t *testing.T) {
	pm := NewPremiumizemeClient("")
	_, err := pm.CreateTransferFromBytes(context.Background(), TransferSourceMagnet, []byte("magnet:test"), "", "folder")
	if !errors.Is(err, ErrTransferNotSubmitted) || !errors.Is(err, ErrAPIKeyNotSet) {
		t.Fatalf("missing API key lost its identity or was classified uncertain: %v", err)
	}
	pm = NewPremiumizemeClient("test-key")
	_, err = pm.CreateTransferFromBytes(context.Background(), TransferSourceMagnet, nil, "", "folder")
	if !errors.Is(err, ErrTransferNotSubmitted) {
		t.Fatalf("invalid local payload was classified uncertain: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err = pm.CreateTransferFromBytes(ctx, TransferSourceMagnet, []byte("magnet:test"), "", "folder")
	if !errors.Is(err, ErrTransferNotSubmitted) || !errors.Is(err, context.Canceled) {
		t.Fatalf("already-cancelled request was classified uncertain: %v", err)
	}
}
