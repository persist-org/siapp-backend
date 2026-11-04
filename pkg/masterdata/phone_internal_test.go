package masterdata

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestNormalizePhone(t *testing.T) {
	good := map[string]string{
		"+628123456789":       "+628123456789",
		"  +62 812-3456-789 ": "+628123456789",
		"08123456789":         "+628123456789",
		"0812 3456 789":       "+628123456789",
		"(0812) 3456.789":     "+628123456789",
		"628123456789":        "+628123456789",
		"00628123456789":      "+628123456789",
		"+6221 555 0101":      "+62215550101",
		"021-555-0101":        "+62215550101",
		"+14155552671":        "+14155552671",
		// National emergency numbers are stored as they are.
		"110": "110", "112": "112", "113": "113", "118": "118", "119": "119",
		" 1 1 0 ": "110",
	}
	for in, want := range good {
		got, ok := normalizePhone(in)
		require.True(t, ok, "%q should be accepted", in)
		require.Equal(t, want, got, "%q", in)
	}

	bad := []string{
		"", "   ", "abc", "08123abc456", "+", "+0812345678", "+62", "0", "08", "1234", "111", "100", "999",
		"+62 812 3456 789 0123 456", // too long for E.164
		"++628123456789", "62+8123456789", "0812345678\n9", "+628123456789;", "tel:+628123456789",
		"٠٨١٢٣٤٥٦٧٨٩", // arabic-indic digits are not digits here
	}
	for _, in := range bad {
		got, ok := normalizePhone(in)
		require.False(t, ok, "%q should be refused, got %q", in, got)
	}
}

func TestCodeFromNameAndFreeCode(t *testing.T) {
	for name, want := range map[string]string{
		"Kebakaran Hutan": "KEBAKARAN_HUTAN", "  Banjir!!  Bandang ": "BANJIR_BANDANG", "112": "P_112", "a": "P_A",
		"火事": "", "!!!": "", "Rumah Sakit (RS)": "RUMAH_SAKIT_RS",
	} {
		require.Equal(t, want, codeFromName(name), name)
	}
	require.Len(t, codeFromName("Kebakaran Hutan Dan Lahan Gambut Yang Sangat Besar"), 30)

	require.Equal(t, "FIRE", freeCode("FIRE", nil))
	require.Equal(t, "FIRE_2", freeCode("FIRE", []string{"FIRE"}))
	require.Equal(t, "FIRE_3", freeCode("FIRE", []string{"FIRE", "FIRE_2"}))
	long := "KEBAKARAN_HUTAN_DAN_LAHAN_GAMBU"[:30]
	got := freeCode(long, []string{long})
	require.LessOrEqual(t, len(got), 30)
	require.NotEqual(t, long, got)
}

func TestCleanName(t *testing.T) {
	got, err := cleanName("  Banyak \t  Spasi\n ")
	require.NoError(t, err)
	require.Equal(t, "Banyak Spasi", got)
	for _, bad := range []string{"", "   ", "\n\t", "bel\u0007l", "nul\u0000"} {
		_, err := cleanName(bad)
		require.Error(t, err, "%q", bad)
	}
}
