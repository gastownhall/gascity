package pidutil

import (
	"bytes"
	"encoding/binary"
	"fmt"
)

// parseProcArgs2 extracts argv from Darwin's kern.procargs2 buffer. The first
// four bytes contain argc, followed by the executable path, NUL padding, argv,
// and environment entries. Counting exactly argc entries keeps environment
// values out of the result.
func parseProcArgs2(data []byte) ([]string, error) {
	argv, _, err := splitProcArgs2(data)
	return argv, err
}

// parseProcArgs2Environ extracts the environment from Darwin's kern.procargs2
// buffer: the strings after the argc arguments, each one whole, so a value
// containing whitespace stays intact. As ps(1) reads it, the environment runs
// to the first empty string or the end of the buffer. A buffer the kernel
// withheld the environment from ends after argv and holds none. The kernel's
// own key=value strings follow the environment after an alignment gap that
// can be empty, so like ps this can return them after its entries.
func parseProcArgs2Environ(data []byte) ([]string, error) {
	_, rest, err := splitProcArgs2(data)
	if err != nil {
		return nil, err
	}
	var env []string
	for len(rest) > 0 && rest[0] != 0 {
		end := bytes.IndexByte(rest, 0)
		if end < 0 {
			return nil, fmt.Errorf("pidutil: procargs2 buffer ended inside environment entry %d", len(env)+1)
		}
		env = append(env, string(rest[:end]))
		rest = rest[end+1:]
	}
	return env, nil
}

// splitProcArgs2 reads argc, the executable path and its NUL padding, and the
// argc arguments off a kern.procargs2 buffer, returning the arguments and the
// bytes after them.
func splitProcArgs2(data []byte) ([]string, []byte, error) {
	const argcSize = 4
	if len(data) < argcSize {
		return nil, nil, fmt.Errorf("pidutil: procargs2 buffer has %d bytes, want at least %d", len(data), argcSize)
	}
	argcValue := binary.LittleEndian.Uint32(data[:argcSize])
	if uint64(argcValue) > uint64(len(data)) {
		return nil, nil, fmt.Errorf("pidutil: procargs2 argc %d exceeds buffer size %d", argcValue, len(data))
	}
	argc := int(argcValue)

	rest := data[argcSize:]
	execEnd := bytes.IndexByte(rest, 0)
	if execEnd < 0 {
		return nil, nil, fmt.Errorf("pidutil: procargs2 buffer has no terminated executable path")
	}
	rest = rest[execEnd+1:]
	for len(rest) > 0 && rest[0] == 0 {
		rest = rest[1:]
	}

	argv := make([]string, 0, argc)
	for len(argv) < argc {
		argEnd := bytes.IndexByte(rest, 0)
		if argEnd < 0 {
			return nil, nil, fmt.Errorf("pidutil: procargs2 buffer ended after %d of %d arguments", len(argv), argc)
		}
		argv = append(argv, string(rest[:argEnd]))
		rest = rest[argEnd+1:]
	}
	return argv, rest, nil
}
