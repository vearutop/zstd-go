//go:build amd64 && !purego

package zstd

// cpuid is the CPUID instruction (cpu_amd64.s).
func cpuid(eaxArg, ecxArg uint32) (eax, ebx, ecx, edx uint32)

// hasBMI1 and hasBMI2 select the assembly variants that use BMI instructions (SHLX, SHRX, BZHI, ...).
// Tests may clear them to exercise the baseline variants.
var hasBMI1, hasBMI2 = detectBMI()

func detectBMI() (bmi1, bmi2 bool) {
	if maxID, _, _, _ := cpuid(0, 0); maxID < 7 {
		return false, false
	}
	_, ebx, _, _ := cpuid(7, 0) //nolint:dogsled // only ebx matters
	return ebx&(1<<3) != 0, ebx&(1<<8) != 0
}
