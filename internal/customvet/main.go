package main

import (
	"github.com/jeevic/go-redis/internal/customvet/checks/setval"
	"golang.org/x/tools/go/analysis/multichecker"
)

func main() {
	multichecker.Main(
		setval.Analyzer,
	)
}
