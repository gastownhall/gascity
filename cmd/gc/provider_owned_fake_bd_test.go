package main

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"time"
)

// nativeFakeBdLogEnv turns a re-executed test binary into a recording bd: each
// run appends one nativeFakeBdCall, as a JSON line, to the file it names. The
// provider under test passes its environment through to bd, so a test drives
// the fake with the same environment it hands the provider.
const (
	nativeFakeBdLogEnv     = "GC_TEST_NATIVE_FAKE_BD_LOG"
	nativeFakeBdRepliesEnv = "GC_TEST_NATIVE_FAKE_BD_REPLIES"
)

// nativeFakeBdCall is what the fake saw of one run.
type nativeFakeBdCall struct {
	At   time.Time `json:"at"`
	Args []string  `json:"args"`
	Dir  string    `json:"dir"`
	Env  []string  `json:"env"`
}

// nativeFakeBdReply is how the fake answers a run. nativeFakeBdRepliesEnv
// holds a JSON object from an argument prefix ("dolt stop") to a reply; the
// longest prefix of the space-joined arguments wins, and no match is a silent
// success. StdoutFile, when set, is a file whose content the fake writes to
// stdout after Stdout, for a reply too large to travel in the environment.
type nativeFakeBdReply struct {
	Exit       int    `json:"exit"`
	Stdout     string `json:"stdout"`
	StdoutFile string `json:"stdout_file"`
	Stderr     string `json:"stderr"`
	SleepMS    int    `json:"sleep_ms"`
}

// maybeRunNativeFakeBd makes this process the fake when the log path is set.
// It must run before the test framework starts, so TestMain calls it first.
func maybeRunNativeFakeBd() {
	logPath := os.Getenv(nativeFakeBdLogEnv)
	if logPath == "" {
		return
	}
	dir, _ := os.Getwd()
	record, err := json.Marshal(nativeFakeBdCall{At: time.Now(), Args: os.Args[1:], Dir: dir, Env: os.Environ()})
	if err == nil {
		err = appendNativeFakeBdRecord(logPath, record)
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "native fake bd: %v\n", err) //nolint:errcheck
		os.Exit(97)
	}
	reply := nativeFakeBdReply{}
	if raw := os.Getenv(nativeFakeBdRepliesEnv); raw != "" {
		var replies map[string]nativeFakeBdReply
		if err := json.Unmarshal([]byte(raw), &replies); err != nil {
			fmt.Fprintf(os.Stderr, "native fake bd: replies: %v\n", err) //nolint:errcheck
			os.Exit(97)
		}
		joined := strings.Join(os.Args[1:], " ")
		best := -1
		for prefix, candidate := range replies {
			if strings.HasPrefix(joined, prefix) && len(prefix) > best {
				best, reply = len(prefix), candidate
			}
		}
	}
	if reply.SleepMS > 0 {
		time.Sleep(time.Duration(reply.SleepMS) * time.Millisecond)
	}
	fmt.Fprint(os.Stdout, reply.Stdout) //nolint:errcheck
	if reply.StdoutFile != "" {
		data, err := os.ReadFile(reply.StdoutFile)
		if err != nil {
			fmt.Fprintf(os.Stderr, "native fake bd: stdout file: %v\n", err) //nolint:errcheck
			os.Exit(97)
		}
		_, _ = os.Stdout.Write(data)
	}
	fmt.Fprint(os.Stderr, reply.Stderr) //nolint:errcheck
	os.Exit(reply.Exit)
}

func appendNativeFakeBdRecord(path string, record []byte) error {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return err
	}
	if _, err := f.Write(append(record, '\n')); err != nil {
		f.Close() //nolint:errcheck
		return err
	}
	return f.Close()
}
