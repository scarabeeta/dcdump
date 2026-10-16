package main

// Console helpers: ask/choose prompts, [ERROR] + exit 1.

import (
	"bufio"
	"fmt"
	"os"
	"strconv"
	"strings"
	"unicode/utf8"
)

var stdin = bufio.NewReader(os.Stdin)

func fatal(msg string) {
	fmt.Println("[ERROR] " + msg)
	os.Exit(1)
}

func readLine() (string, bool) {
	s, err := stdin.ReadString('\n')
	if err != nil && s == "" {
		return "", false
	}
	return strings.TrimSpace(s), true
}

func ask(prompt, def string) string {
	if def != "" {
		fmt.Printf("%s [%s]: ", prompt, def)
	} else {
		fmt.Printf("%s: ", prompt)
	}
	s, ok := readLine()
	if !ok {
		fmt.Println()
		fatal("No input available (non-interactive session). Use --drive to pick the right drive so serial/name can be auto-detected, or run interactively with a tty.")
	}
	if s == "" {
		return def
	}
	return s
}

func choose(prompt string, items []string, def int) int {
	printList(items, def)
	for {
		fmt.Printf("%s [1-%d] (Enter=%d): ", prompt, len(items), def+1)
		s, ok := readLine()
		if !ok {
			fmt.Println()
			fatal("No input available (non-interactive session). Use --drive to pick the right drive, or run interactively with a tty.")
		}
		if s == "" {
			return def
		}
		n, err := strconv.Atoi(s)
		if err != nil {
			fmt.Println()
			os.Exit(0)
		}
		if n >= 1 && n <= len(items) {
			return n - 1
		}
	}
}

// box prints the ═ framed notice used for things that must not be overlooked.
func box(lines []string) {
	w := 0
	for _, l := range lines {
		if n := utf8.RuneCountInString(l); n > w {
			w = n
		}
	}
	rule := strings.Repeat("═", w+4)
	fmt.Println(rule)
	for _, l := range lines {
		fmt.Println("  " + l)
	}
	fmt.Println(rule)
}

func banner(title string) string {
	return "  " + strings.Repeat("─", utf8.RuneCountInString(title))
}
