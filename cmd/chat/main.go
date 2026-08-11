// Command chat starts the terminal diagnostic assistant.
package main

import (
	"os"

	"diagnostic-system/internal/application"
	"diagnostic-system/internal/ui"
)

func main() {
	if err := application.Run(); err != nil {
		_ = ui.ShowError(err)
		os.Exit(1)
	}
}
