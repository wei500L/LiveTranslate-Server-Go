// Command livetranslate-server is the single binary for the LiveTranslate
// cloud sync backend.
//
//	livetranslate-server serve        # the /v1 API (iOS clients)
//	livetranslate-server admin        # the admin web UI (its own listener)
//	livetranslate-server create-admin # create an admin account (interactive)
//	livetranslate-server enable-totp <username> # print a TOTP secret for an admin
//	livetranslate-server migrate      # apply migrations and exit
package main

import (
	"fmt"
	"os"
	"strings"
)

func main() {
	cmd := "serve"
	if len(os.Args) > 1 {
		cmd = os.Args[1]
	}
	var err error
	switch cmd {
	case "serve":
		err = runServe()
	case "admin":
		err = runAdmin()
	case "create-admin":
		err = runCreateAdmin(os.Args[2:])
	case "enable-totp":
		err = runEnableTOTP(os.Args[2:])
	case "migrate":
		err = runMigrate()
	case "-h", "--help", "help":
		usage()
	default:
		usage()
		os.Exit(2)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

func usage() {
	name := os.Args[0]
	fmt.Fprintf(os.Stderr, strings.TrimSpace(usageText)+"\n", name)
}

const usageText = `
usage:
  %[1]s serve         run the /v1 API server (LISTEN_ADDR, default 127.0.0.1:8000)
  %[1]s admin         run the admin web UI (ADMIN_LISTEN_ADDR, default 127.0.0.1:8081)
  %[1]s create-admin  create an admin account (prompts for username/password)
  %[1]s enable-totp <username>
                     generate and enable a TOTP secret for an admin
  %[1]s migrate       apply database migrations and exit
`
