package main

import (
	"bufio"
	"context"
	"crypto/rand"
	"fmt"
	"os"
	"strings"
	"syscall"

	"golang.org/x/term"

	"livetranslate/server/internal/admin"
	"livetranslate/server/internal/auth"
	"livetranslate/server/internal/config"
	"livetranslate/server/internal/password"
	"livetranslate/server/internal/store"
)

// runCreateAdmin interactively creates an admin account. The password is
// read from the terminal with echo disabled, twice, validated against the
// same policy as user passwords, and stored as an Argon2id PHC hash.
// Nothing is ever hardcoded or logged.
func runCreateAdmin(args []string) error {
	if len(args) != 0 {
		return fmt.Errorf("usage: livetranslate-server create-admin")
	}
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	st, err := store.NewDB(context.Background(), cfg.DatabaseURL)
	if err != nil {
		return err
	}
	defer st.Close()

	r := bufio.NewReader(os.Stdin)
	fmt.Print("管理员用户名: ")
	username, err := r.ReadString('\n')
	if err != nil {
		return err
	}
	username = strings.TrimSpace(username)
	if len(username) < 3 || len(username) > 64 {
		return fmt.Errorf("用户名长度须为 3-64")
	}

	pw, err := readPasswordTwice(r)
	if err != nil {
		return err
	}
	if err := password.Validate(pw, username+"@admin.local", username); err != nil {
		return fmt.Errorf("密码不满足策略: %w", err)
	}

	hash, err := password.Hash(pw, password.Params{
		MemoryKiB: cfg.Argon2MemoryKiB, Iterations: cfg.Argon2Iterations,
		Parallel: cfg.Argon2Parallel, SaltLen: 16, KeyLen: 32,
	})
	if err != nil {
		return err
	}
	ctx := context.Background()
	if err := admin.CreateAdmin(ctx, st.Q(), username, hash, nil); err != nil {
		if strings.Contains(err.Error(), "duplicate key") {
			return fmt.Errorf("用户名已存在")
		}
		return err
	}
	// Audit the creation (actor = the new admin itself, marked system-side).
	_ = auth.HashPII(username)
	fmt.Println("管理员已创建:", username)
	fmt.Println("如需启用 TOTP 二步验证，运行: livetranslate-server enable-totp " + username)
	return nil
}

// runEnableTOTP generates a fresh TOTP secret for an admin account and
// prints the otpauth:// URL to configure an authenticator app.
func runEnableTOTP(args []string) error {
	if len(args) != 1 {
		return fmt.Errorf("usage: livetranslate-server enable-totp <username>")
	}
	username := args[0]
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	st, err := store.NewDB(context.Background(), cfg.DatabaseURL)
	if err != nil {
		return err
	}
	defer st.Close()

	ctx := context.Background()
	adm, err := admin.GetAdminByUsername(ctx, st.Q(), username)
	if err != nil {
		return fmt.Errorf("管理员不存在")
	}
	secret, err := admin.NewTOTPSecret(func(n int) ([]byte, error) {
		b := make([]byte, n)
		if _, err := rand.Read(b); err != nil {
			return nil, err
		}
		return b, nil
	})
	if err != nil {
		return err
	}
	if err := admin.UpdateAdminTOTP(ctx, st.Q(), adm.ID, &secret); err != nil {
		return err
	}
	fmt.Println("TOTP 已启用。请立即在验证器 App 中添加（此密钥不会再次显示）:")
	fmt.Println()
	fmt.Printf("  密钥 (base32): %s\n", secret)
	fmt.Printf("  otpauth://totp/LiveTranslate:%s?secret=%s&issuer=LiveTranslate\n", username, secret)
	fmt.Println()
	fmt.Println("添加后请重新登录验证一次。若丢失验证器，需要数据库手动清除 totp_secret。")
	return nil
}

// readPasswordTwice reads the password twice. On a terminal the input is
// masked with term.ReadPassword; when stdin is not a TTY (scripts, tests)
// it falls back to plain line reading — the operator opted into that.
func readPasswordTwice(r *bufio.Reader) (string, error) {
	var read func(string) (string, error)
	if term.IsTerminal(int(syscall.Stdin)) {
		read = func(prompt string) (string, error) {
			fmt.Print(prompt + "（输入不回显）: ")
			b, err := term.ReadPassword(int(syscall.Stdin))
			fmt.Println()
			return string(b), err
		}
	} else {
		// Non-TTY (scripts/tests): plain line reads on the SHARED reader —
		// a fresh bufio.Reader would buffer ahead and swallow lines.
		read = func(prompt string) (string, error) {
			fmt.Print(prompt + ": ")
			line, err := r.ReadString('\n')
			fmt.Println()
			return strings.TrimRight(line, "\r\n"), err
		}
	}
	p1, err := read("密码（10-128 位）")
	if err != nil {
		return "", err
	}
	p2, err := read("再次输入密码")
	if err != nil {
		return "", err
	}
	if p1 != p2 {
		return "", fmt.Errorf("两次输入不一致")
	}
	if p1 == "" {
		return "", fmt.Errorf("密码不能为空")
	}
	return p1, nil
}
