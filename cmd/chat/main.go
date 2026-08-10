// Command chat starts the terminal diagnostic assistant.
package main

import (
	"os"

	"diagnostic-system/internal/chat"
)

func main() {
	if err := chat.Run(); err != nil {
		_ = chat.ShowError(err)
		os.Exit(1)
	}
}
