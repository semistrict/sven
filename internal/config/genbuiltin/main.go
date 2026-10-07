// Command genbuiltin adds the built-in rules' questions to a file, the free
// sven API's allow-list. It never removes one, so sven installs that still
// ask an older wording keep getting answers.
package main

import (
	"errors"
	"io/fs"
	"log"
	"os"

	"github.com/semistrict/sven/internal/config"
)

func main() {
	if len(os.Args) != 2 {
		log.Fatal("usage: genbuiltin <allow-list file>")
	}
	old, err := os.ReadFile(os.Args[1])
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		log.Fatal(err)
	}
	out, err := config.AllowList(old)
	if err != nil {
		log.Fatal(err)
	}
	if err := os.WriteFile(os.Args[1], out, 0o644); err != nil {
		log.Fatal(err)
	}
}
