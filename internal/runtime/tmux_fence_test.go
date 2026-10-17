package runtime

import (
	"slices"
	"testing"
)

// Pins the guard's exact if-shell: the condition requires the named pane,
// its own fence and a live pane; the else branch deletes the staged buffer
// and reports what it found; every argument is single-quoted.
func TestTmuxGuardedInputArgsGolden(t *testing.T) {
	got, err := TmuxGuardedInputArgs("=lab:@1.%1", "%1", "1172ccc4fbf9f6a6", []string{"paste-buffer", "-d", "-r", "-b", "gc-nudge-1-2", "-t", "=lab:@1.%1"}, "gc-nudge-1-2")
	if err != nil {
		t.Fatal(err)
	}
	want := []string{
		"if-shell", "-F", "-t", "=lab:@1.%1",
		"#{&&:#{==:#{pane_id},%1},#{&&:#{==:#{@gc_fence},1172ccc4fbf9f6a6.#{pane_id}},#{!=:#{pane_dead},1}}}",
		"'paste-buffer' '-d' '-r' '-b' 'gc-nudge-1-2' '-t' '=lab:@1.%1'",
		"'delete-buffer' '-b' 'gc-nudge-1-2' ; 'display-message' '-p' '-t' '=lab:@1.%1' 'gc-fence-miss:#{@gc_fence}|#{pane_id}|#{pane_dead}'",
	}
	if !slices.Equal(got, want) {
		t.Fatalf("TmuxGuardedInputArgs =\n%q\nwant\n%q", got, want)
	}
	keys, err := TmuxGuardedInputArgs("=lab:@1.%1", "%1", "1172ccc4fbf9f6a6", []string{"send-keys", "-t", "=lab:@1.%1", "Enter"}, "")
	if err != nil {
		t.Fatal(err)
	}
	if keys[6] != "'display-message' '-p' '-t' '=lab:@1.%1' 'gc-fence-miss:#{@gc_fence}|#{pane_id}|#{pane_dead}'" {
		t.Fatalf("keys else branch = %q, want only the miss report", keys[6])
	}
}

// Kills a guard that lets text or a format into a command string tmux
// parses, or that guards on something that is not a digest or a pane id: a
// quote, a control byte, a format and an empty argument are refused, as are
// a digest that is not one and a pane that is not a pane id.
func TestTmuxGuardedInputArgsRefusesUnsafeArgs(t *testing.T) {
	for _, arg := range []string{"it's", "a\nb", "a\x1bb", "a\x7f", "#{pane_pid}", "x#{@gc_fence}", ""} {
		if _, err := TmuxGuardedInputArgs("=lab:@1.%1", "%1", "1172ccc4fbf9f6a6", []string{"send-keys", "-t", "=lab:@1.%1", arg}, ""); err == nil {
			t.Errorf("argument %q was accepted", arg)
		}
	}
	if _, err := TmuxGuardedInputArgs("=lab:@1.%1", "%1", "abc}", []string{"send-keys", "-t", "=lab:@1.%1", "Enter"}, ""); err == nil {
		t.Error("an invalid digest was accepted")
	}
	for _, pane := range []string{"", "%", "1", "%1a", "=lab:@1.%1"} {
		if _, err := TmuxGuardedInputArgs("=lab:@1.%1", pane, "1172ccc4fbf9f6a6", []string{"send-keys", "-t", "=lab:@1.%1", "Enter"}, ""); err == nil {
			t.Errorf("pane %q was accepted", pane)
		}
	}
}

// Kills a fence read parser that accepts a pane field that is not a pane id
// or that misreads a dead pane, an absent session or a fence inherited from
// another pane.
func TestParseTmuxFenceRead(t *testing.T) {
	for out, want := range map[string]struct {
		ok    bool
		found string
	}{
		"d.%3|%3|0":  {true, "d"},
		"d.%3|%3|1":  {true, FenceFoundDead},
		"d.%9|%3|0":  {true, FenceFoundUnproven},
		"|%3|0":      {true, FenceFoundUnproven},
		"||":         {true, FenceFoundAbsent},
		"d.x|x|0":    {false, ""},
		"d.%3|%3a|0": {false, ""},
		"d|%3":       {false, ""},
	} {
		r, ok := ParseTmuxFenceRead(out)
		if ok != want.ok || (ok && r.Found() != want.found) {
			t.Errorf("ParseTmuxFenceRead(%q) = %+v, %v; want ok %v finding %q", out, r, ok, want.ok, want.found)
		}
	}
}

// Kills a guard format check that accepts any value ending in the pane-id
// format: only a fence value whose digest is a key name, and the guard's own
// miss report, are formats a guarded command may carry.
func TestTmuxGuardFormatArg(t *testing.T) {
	for a, want := range map[string]bool{
		TmuxFenceValue("1172ccc4fbf9f6a6"):        true,
		TmuxFenceMissPrefix + TmuxFenceReadFormat: true,
		"#{pane_pid}.#{pane_id}":                  false,
		"a b.#{pane_id}":                          false,
		".#{pane_id}":                             false,
		"1172ccc4fbf9f6a6.#{pane_pid}":            false,
		TmuxFenceMissPrefix + "#{pane_pid}":       false,
	} {
		if got := TmuxGuardFormatArg(a); got != want {
			t.Errorf("TmuxGuardFormatArg(%q) = %v, want %v", a, got, want)
		}
	}
}
