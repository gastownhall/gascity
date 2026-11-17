package rppcheck

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRun_ProgramStatusExitTwoOrMalformedFails(t *testing.T) {
	for _, answer := range []string{`exit 2`, `echo '{"state":'`} {
		body := conformantScript(t.TempDir())
		body = strings.Replace(body, `"report-activity"]`, `"report-activity","program-status"]`, 1)
		body = strings.Replace(body, `  *) exit 2 ;;`, `  program-status) `+answer+` ;;
  *) exit 2 ;;`, 1)
		result := runScript(t, body, Options{})
		if check := findCheck(t, result, "capability program-status: program-status"); check.Status != StatusFail {
			t.Errorf("%s: %+v", answer, check)
		}
	}
}

func TestRun_ProgramStatusUndeclaredSendsNothing(t *testing.T) {
	for _, failedStart := range []bool{false, true} {
		dir := t.TempDir()
		log := filepath.Join(dir, "ops")
		body := conformantScript(dir)
		if failedStart {
			body = strings.Replace(body, `touch "$state/$name.running"`, `exit 1`, 1)
		}
		body = fmt.Sprintf("printf '%%s\\n' \"$1\" >> %q\n", log) + body
		result := runScript(t, body, Options{})
		if hasCheck(result, "capability program-status: program-status") || hasCheck(result, "optional: program-status") {
			t.Error("undeclared token added a check")
		}
		data, err := os.ReadFile(log)
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(data), "program-status") {
			t.Error("undeclared op was sent")
		}
	}
}

func TestRun_ProgramStatusDeclaredIsChecked(t *testing.T) {
	body := conformantScript(t.TempDir())
	body = strings.Replace(body, `"report-activity"]`, `"report-activity","program-status"]`, 1)
	body = strings.Replace(body, `  *) exit 2 ;;`, `  program-status) echo '{"state":"idle"}' ;;
  *) exit 2 ;;`, 1)
	result := runScript(t, body, Options{})
	const name = "capability program-status: program-status"
	if !hasCheck(result, name) {
		t.Fatal("declared program-status was not checked")
	}
	if c := findCheck(t, result, name); c.Status != StatusPass {
		t.Errorf("%+v", c)
	}
}
