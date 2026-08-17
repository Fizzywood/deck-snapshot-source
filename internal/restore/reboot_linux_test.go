//go:build linux

package restore

import (
	"context"
	"errors"
	"testing"
)

type fakeLogindClient struct {
	capability  string
	canErr      error
	rebootErr   error
	rebootCalls int
	interactive bool
}

func (client *fakeLogindClient) CanReboot(context.Context) (string, error) {
	return client.capability, client.canErr
}

func (client *fakeLogindClient) Reboot(_ context.Context, interactive bool) error {
	client.rebootCalls++
	client.interactive = interactive
	return client.rebootErr
}

func TestSessionRebooterPreflightCapabilities(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name       string
		capability string
		wantErr    bool
	}{
		{name: "yes", capability: "yes"},
		{name: "challenge", capability: "challenge"},
		{name: "no", capability: "no", wantErr: true},
		{name: "not applicable", capability: "na", wantErr: true},
		{name: "malformed", capability: "unexpected", wantErr: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			client := &fakeLogindClient{capability: test.capability}
			err := (sessionRebooter{logind: client}).Preflight(context.Background())
			if (err != nil) != test.wantErr {
				t.Fatalf("Preflight() error = %v, want error = %v", err, test.wantErr)
			}
			if client.rebootCalls != 0 {
				t.Fatalf("Preflight() called Reboot %d times", client.rebootCalls)
			}
		})
	}
}

func TestSessionRebooterPreflightUnavailable(t *testing.T) {
	t.Parallel()
	client := &fakeLogindClient{canErr: errors.New("service unavailable")}
	if err := (sessionRebooter{logind: client}).Preflight(context.Background()); err == nil {
		t.Fatal("Preflight() unexpectedly succeeded")
	}
	if client.rebootCalls != 0 {
		t.Fatal("Preflight() called Reboot")
	}
}

func TestSessionRebooterRequestUsesInteractiveLogindReboot(t *testing.T) {
	t.Parallel()
	client := &fakeLogindClient{}
	if err := (sessionRebooter{logind: client}).Request(context.Background()); err != nil {
		t.Fatalf("Request() error = %v", err)
	}
	if client.rebootCalls != 1 || !client.interactive {
		t.Fatalf("Request() calls = %d, interactive = %v", client.rebootCalls, client.interactive)
	}
}

func TestSessionRebooterRequestFailure(t *testing.T) {
	t.Parallel()
	client := &fakeLogindClient{rebootErr: errors.New("authorization failed")}
	if err := (sessionRebooter{logind: client}).Request(context.Background()); err == nil {
		t.Fatal("Request() unexpectedly succeeded")
	}
	if client.rebootCalls != 1 {
		t.Fatalf("Request() calls = %d, want 1", client.rebootCalls)
	}
}

func TestParseLogindString(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		input   string
		want    string
		wantErr bool
	}{
		{input: "s \"challenge\"\n", want: "challenge"},
		{input: "s \"yes\"", want: "yes"},
		{input: "b true", wantErr: true},
		{input: "s challenge", wantErr: true},
		{input: "s \"\"", wantErr: true},
	} {
		got, err := parseLogindString([]byte(test.input))
		if (err != nil) != test.wantErr || got != test.want {
			t.Errorf("parseLogindString(%q) = (%q, %v), want (%q, error=%v)", test.input, got, err, test.want, test.wantErr)
		}
	}
}
