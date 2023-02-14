package setval_test

import (
	"testing"

	"github.com/jeevic/go-redis/internal/customvet/checks/setval"
	"golang.org/x/tools/go/analysis/analysistest"
)

func Test(t *testing.T) {
	testdata := analysistest.TestData()
	analysistest.Run(t, testdata, setval.Analyzer, "a")
}
