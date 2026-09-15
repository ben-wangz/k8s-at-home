package observability

import (
	"io"
	"os"
)

// stdoutWriter is an indirection so tests can capture logger output.
var stdoutWriter io.Writer = os.Stdout
