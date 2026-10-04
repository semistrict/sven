// Command genbuiltin writes the built-in rules' questions to a file, for the
// free sven API's allow-list.
package main

import (
	"log"
	"os"

	"github.com/semistrict/sven/internal/config"
)

func main() {
	if len(os.Args) != 2 {
		log.Fatal("usage: genbuiltin <output file>")
	}
	out, err := config.BuiltinQuestions()
	if err != nil {
		log.Fatal(err)
	}
	if err := os.WriteFile(os.Args[1], out, 0o644); err != nil {
		log.Fatal(err)
	}
}
