package toolvm

import (
	"fmt"
	"strings"
	"testing"
)

// The header is embedded so that `nine tool header` always emits the contract
// the running binary actually implements. That guarantee is only worth
// something if the version baked into the C matches the one enforced by the Go,
// so this asserts they agree. A bump to ABIVersion that forgets nine.h would
// otherwise ship a header telling authors to declare an abi the loader refuses.
func TestCHeaderMatchesABIVersion(t *testing.T) {
	want := fmt.Sprintf("#define NINE_ABI_VERSION %d", ABIVersion)
	if !strings.Contains(CHeader, want) {
		t.Errorf("nine.h does not define NINE_ABI_VERSION as %d\nlooked for: %q",
			ABIVersion, want)
	}
}

// The two exports are the whole ABI. A header that stopped naming one of them
// would still compile for the author who does not use the macro, and fail at
// load with "module does not export the Nine ABI".
func TestCHeaderNamesTheABI(t *testing.T) {
	for _, want := range []string{exportAlloc, exportRun, hostModule} {
		if !strings.Contains(CHeader, want) {
			t.Errorf("nine.h never mentions %q", want)
		}
	}
}
