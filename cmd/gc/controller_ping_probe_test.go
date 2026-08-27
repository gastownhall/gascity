package main

import (
	"bufio"
	"net"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// TestControllerPingProbeDoesNotRequestATick pins the property `gc session
// suspend`'s managed-mode probe relies on: "ping" proves a controller is serving
// without queueing a reconciler tick. The suspend probe runs BEFORE the durable
// suspend patch, so a probe that ticks hands the controller a pre-patch snapshot
// to reconcile from — whose advisory status heal then reverts the user's suspend
// marker (ga-f7v2ft.125). "poke" is the contrasting control.
func TestControllerPingProbeDoesNotRequestATick(t *testing.T) {
	for _, test := range []struct {
		command  string
		wantPoke int
	}{
		{command: "ping", wantPoke: 0},
		{command: "poke", wantPoke: 1},
	} {
		t.Run(test.command, func(t *testing.T) {
			server, client := net.Pipe()
			pokeCh := make(chan struct{}, 1)
			dirty := &atomic.Bool{}
			done := make(chan struct{})
			go func() {
				defer close(done)
				handleControllerConn(server, t.TempDir(), controllerHostingStandalone, func() {}, dirty, nil, pokeCh, nil, nil)
			}()

			client.SetDeadline(time.Now().Add(10 * time.Second)) //nolint:errcheck // test connection
			if _, err := client.Write([]byte(test.command + "\n")); err != nil {
				t.Fatalf("send %q: %v", test.command, err)
			}
			reply, err := bufio.NewReader(client).ReadString('\n')
			if err != nil {
				t.Fatalf("read %q reply: %v", test.command, err)
			}
			client.Close() //nolint:errcheck // test connection
			<-done

			if got := len(pokeCh); got != test.wantPoke {
				t.Fatalf("%q queued %d reconciler ticks, want %d", test.command, got, test.wantPoke)
			}
			if dirty.Load() {
				t.Fatalf("%q marked the config dirty", test.command)
			}
			reply = strings.TrimSpace(reply)
			if test.command == "ping" {
				if pid, convErr := strconv.Atoi(reply); convErr != nil || pid <= 0 {
					t.Fatalf("ping reply = %q, want the serving PID", reply)
				}
			}
		})
	}
}
