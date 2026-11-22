package main

import (
	"log/slog"
	"os"

	"github.com/GMWalletApp/BalanceKeeper/src"
)

func main() {
	if err := balancekeeper.Run(); err != nil {
		slog.Error("BalanceKeeper stopped", "error", err)
		os.Exit(1)
	}
}
