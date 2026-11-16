package genshell

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"slices"
	"strings"

	"github.com/itchyny/gojq"
)

// The exit statuses jq reports.
const (
	jqStatusUsage   = 2 // a bad command line, or input that is not JSON
	jqStatusCompile = 3 // a filter that does not compile
	jqStatusRuntime = 5 // a filter that failed on an input
)

// runJQ runs the part of jq's command line that generated programs use,
//
//	jq [-r] [-c] [-s] [-n] [--arg NAME TEXT] [--argjson NAME JSON] FILTER
//
// with the long forms of the four flags, and short flags combined as in -rc.
// Any other option, or an input file, is a bad command line.
//
// The input is the stream of JSON values on stdin; a nil stdin is empty
// input. As in jq, each value is filtered in turn (all at once, as one array,
// under -s; once, with null input, under -n), and each result is written on
// its own line: indented by two spaces, or compact under -c, with a string
// written bare under -r. Nothing is HTML-escaped. Unlike jq, an object's keys
// are written in sorted order. --arg defines $NAME as the string TEXT and
// --argjson defines it as the JSON value JSON.
//
// It returns jq's exit status: 0; 2 for a bad command line or input that is
// not JSON; 3 for a filter that does not compile; 5 when the filter failed on
// an input, after which the following inputs are still filtered; or the
// status halt or halt_error set.
func runJQ(ctx context.Context, args []string, stdin io.Reader, stdout, stderr io.Writer, environ []string) uint8 {
	var (
		filter                         string
		haveFilter                     bool
		raw, compact, slurp, nullInput bool
		names                          []string
		values                         []any
	)
	define := func(name string, value any) {
		name = "$" + name
		if i := slices.Index(names, name); i >= 0 {
			values[i] = value
			return
		}
		names = append(names, name)
		values = append(values, value)
	}
	for i := 0; i < len(args); i++ {
		arg := args[i]
		switch {
		case arg == "--arg" || arg == "--argjson":
			if i+2 >= len(args) {
				fmt.Fprintf(stderr, "jq: %s takes two parameters (e.g. %s varname value)\n", arg, arg) //nolint:errcheck // best-effort stderr
				return jqStatusUsage
			}
			name, text := args[i+1], args[i+2]
			i += 2
			if arg == "--arg" {
				define(name, text)
				continue
			}
			value, err := decodeJSONText(text)
			if err != nil {
				fmt.Fprintf(stderr, "jq: invalid JSON text passed to --argjson: %v\n", err) //nolint:errcheck // best-effort stderr
				return jqStatusUsage
			}
			define(name, value)
		case arg == "--raw-output":
			raw = true
		case arg == "--compact-output":
			compact = true
		case arg == "--slurp":
			slurp = true
		case arg == "--null-input":
			nullInput = true
		case len(arg) > 1 && arg[0] == '-' && arg[1] != '-':
			for _, flag := range arg[1:] {
				switch flag {
				case 'r':
					raw = true
				case 'c':
					compact = true
				case 's':
					slurp = true
				case 'n':
					nullInput = true
				default:
					fmt.Fprintf(stderr, "jq: unsupported option: %s\n", arg) //nolint:errcheck // best-effort stderr
					return jqStatusUsage
				}
			}
		case strings.HasPrefix(arg, "-") && len(arg) > 1:
			fmt.Fprintf(stderr, "jq: unsupported option: %s\n", arg) //nolint:errcheck // best-effort stderr
			return jqStatusUsage
		case !haveFilter:
			filter, haveFilter = arg, true
		default:
			fmt.Fprintf(stderr, "jq: input files are not supported: %s\n", arg) //nolint:errcheck // best-effort stderr
			return jqStatusUsage
		}
	}
	if !haveFilter {
		fmt.Fprintln(stderr, "jq: no filter given") //nolint:errcheck // best-effort stderr
		return jqStatusUsage
	}

	query, err := gojq.Parse(filter)
	if err != nil {
		fmt.Fprintf(stderr, "jq: error: %v\njq: 1 compile error\n", err) //nolint:errcheck // best-effort stderr
		return jqStatusCompile
	}
	code, err := gojq.Compile(query,
		gojq.WithVariables(names),
		gojq.WithEnvironLoader(func() []string { return environ }),
	)
	if err != nil {
		fmt.Fprintf(stderr, "jq: error: %v\njq: 1 compile error\n", err) //nolint:errcheck // best-effort stderr
		return jqStatusCompile
	}

	var status uint8
	// filterInput writes the results of filtering one input and reports
	// whether to go on to the next.
	filterInput := func(input any) bool {
		iter := code.RunWithContext(ctx, input, values...)
		for {
			v, ok := iter.Next()
			if !ok {
				return true
			}
			err, isErr := v.(error)
			if !isErr {
				writeJQValue(stdout, v, raw, compact)
				continue
			}
			if ctx.Err() != nil {
				return false
			}
			var halt *gojq.HaltError
			if errors.As(err, &halt) {
				if value := halt.Value(); value != nil {
					if text, ok := value.(string); ok {
						io.WriteString(stderr, text) //nolint:errcheck // best-effort stderr
					} else {
						writeJQValue(stderr, value, false, true)
					}
				}
				status = statusFromJQ(halt.ExitCode())
				return false
			}
			fmt.Fprintf(stderr, "jq: error (at <stdin>): %v\n", err) //nolint:errcheck // best-effort stderr
			status = jqStatusRuntime
			return true
		}
	}

	if nullInput {
		filterInput(nil)
		return status
	}
	var slurped []any
	if stdin != nil {
		dec := json.NewDecoder(stdin)
		dec.UseNumber()
		for {
			var input any
			err := dec.Decode(&input)
			if errors.Is(err, io.EOF) {
				break
			}
			if err != nil {
				fmt.Fprintf(stderr, "jq: error (at <stdin>): cannot parse input: %v\n", err) //nolint:errcheck // best-effort stderr
				return jqStatusUsage
			}
			if slurp {
				slurped = append(slurped, input)
				continue
			}
			if !filterInput(input) {
				return status
			}
		}
	}
	if slurp {
		if slurped == nil {
			slurped = []any{}
		}
		filterInput(slurped)
	}
	return status
}

// writeJQValue writes v on its own line as jq does.
func writeJQValue(w io.Writer, v any, raw, compact bool) {
	if text, ok := v.(string); ok && raw {
		io.WriteString(w, text+"\n") //nolint:errcheck // a closed reader ends the pipeline, as for jq
		return
	}
	text, _ := gojq.Marshal(v)
	if !compact {
		var indented bytes.Buffer
		if json.Indent(&indented, text, "", "  ") == nil {
			text = indented.Bytes()
		}
	}
	w.Write(append(text, '\n')) //nolint:errcheck // a closed reader ends the pipeline, as for jq
}

// decodeJSONText decodes text, which must hold exactly one JSON value.
func decodeJSONText(text string) (any, error) {
	dec := json.NewDecoder(strings.NewReader(text))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		return nil, err
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return nil, errors.New("more than one JSON value")
	}
	return v, nil
}

// statusFromJQ maps the exit code halt or halt_error set onto an exit status.
func statusFromJQ(code int) uint8 {
	if code < 0 || code > 255 {
		return 255
	}
	return uint8(code)
}
