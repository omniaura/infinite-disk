package cli

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"

	"github.com/omniaura/infinite-disk/internal/disk"
)

// WriteError keeps successful/partial results on stdout and one machine-readable
// error on stderr. A failed apply can already have preserved source: inspect its
// receipt rather than interpreting a nonzero exit as proof of no side effects.
func WriteError(w io.Writer, err error, asJSON, cancelled bool) {
	code := "command_failed"
	message := err.Error()
	if errors.Is(err, disk.ErrOperationInProgress) {
		code = "operation_in_progress"
	}
	if cancelled {
		code = "cancelled"
		message = "Cancelled. No further actions will start; check receipts if an action had begun."
	}
	if asJSON {
		_ = json.NewEncoder(w).Encode(map[string]any{"version": 1, "error": map[string]string{"code": code, "message": message}})
		return
	}
	fmt.Fprintln(w, "Error:", message)
}
