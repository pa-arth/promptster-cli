package main

import (
	"fmt"
	"os"
)

func cmdDecide(_ []string) {
	fmt.Fprintln(os.Stderr, "'promptster decide' has been replaced by 'promptster explain'.")
	fmt.Fprintln(os.Stderr, "Run 'promptster explain' to document your decision rationale.")
	os.Exit(1)
}
