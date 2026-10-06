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
	const argcSize = 4
	if len(data) < argcSize {
		return nil, fmt.Errorf("pidutil: procargs2 buffer has %d bytes, want at least %d", len(data), argcSize)
	}
	argcValue := binary.LittleEndian.Uint32(data[:argcSize])
	if uint64(argcValue) > uint64(len(data)) {
		return nil, fmt.Errorf("pidutil: procargs2 argc %d exceeds buffer size %d", argcValue, len(data))
	}
	argc := int(argcValue)

	rest := data[argcSize:]
	execEnd := bytes.IndexByte(rest, 0)
	if execEnd < 0 {
		return nil, fmt.Errorf("pidutil: procargs2 buffer has no terminated executable path")
	}
	rest = rest[execEnd+1:]
	for len(rest) > 0 && rest[0] == 0 {
		rest = rest[1:]
	}

	argv := make([]string, 0, argc)
	for len(argv) < argc {
		argEnd := bytes.IndexByte(rest, 0)
		if argEnd < 0 {
			return nil, fmt.Errorf("pidutil: procargs2 buffer ended after %d of %d arguments", len(argv), argc)
		}
		argv = append(argv, string(rest[:argEnd]))
		rest = rest[argEnd+1:]
	}
	return argv, nil
}
