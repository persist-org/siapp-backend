package organization_test

import (
	"testing"

	"github.com/stretchr/testify/require"

	"siapp/pkg/organization"
)

func TestBuildBranchCode(t *testing.T) {
	for _, tc := range []struct {
		name, region, letter, want, reason string
	}{
		{name: "KPI under OS1", region: "OS1", letter: "K", want: "1K"},
		{name: "IDS under OS2", region: "OS2", letter: "I", want: "2I"},
		{name: "PCD under OS12", region: "OS12", letter: "P", want: "12P"},
		{name: "another letter", region: "OS1", letter: "X", reason: organization.ReasonBranchLetterInvalid},
		{name: "a lower case letter", region: "OS1", letter: "k", reason: organization.ReasonBranchLetterInvalid},
		{name: "no letter", region: "OS1", letter: "", reason: organization.ReasonBranchLetterInvalid},
		{name: "two letters", region: "OS1", letter: "KI", reason: organization.ReasonBranchLetterInvalid},
		{name: "a region that is not one", region: "1K", letter: "K"},
		{name: "a region with a leading zero", region: "OS01", letter: "K"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := organization.BuildBranchCode(tc.region, tc.letter)
			if tc.want == "" {
				require.Error(t, err)
				if tc.reason != "" {
					require.ErrorContains(t, err, tc.reason)
				}
				return
			}
			require.NoError(t, err)
			require.Equal(t, tc.want, got)
		})
	}
}

func TestValidateBranchCode(t *testing.T) {
	for _, tc := range []struct {
		name, region, code, reason string
	}{
		{name: "1K under OS1", region: "OS1", code: "1K"},
		{name: "12P under OS12", region: "OS12", code: "12P"},
		{name: "2K under OS1", region: "OS1", code: "2K", reason: organization.ReasonBranchRegionMismatch},
		{name: "1K under OS11", region: "OS11", code: "1K", reason: organization.ReasonBranchRegionMismatch},
		{name: "11K under OS1", region: "OS1", code: "11K", reason: organization.ReasonBranchRegionMismatch},
		{name: "letter X", region: "OS1", code: "1X", reason: organization.ReasonBranchLetterInvalid},
		{name: "no letter", region: "OS1", code: "1", reason: organization.ReasonBranchLetterInvalid},
		{name: "no number", region: "OS1", code: "K", reason: organization.ReasonBranchLetterInvalid},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := organization.ValidateBranchCode(tc.region, tc.code)
			if tc.reason == "" {
				require.NoError(t, err)
				return
			}
			require.ErrorContains(t, err, tc.reason)
		})
	}
}

func TestBuildClientCode(t *testing.T) {
	for _, tc := range []struct {
		name, prefix, want string
	}{
		{name: "letters", prefix: "IBMI", want: "1K-IBMI"},
		{name: "letters and digits", prefix: "A1", want: "1K-A1"},
		{name: "the longest", prefix: "ABCDEFGHI9", want: "1K-ABCDEFGHI9"},
		{name: "too short", prefix: "A"},
		{name: "too long", prefix: "ABCDEFGHIJK"},
		{name: "lower case", prefix: "ibmi"},
		{name: "a dash", prefix: "IB-MI"},
		{name: "a space", prefix: "IB MI"},
		{name: "empty", prefix: ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := organization.BuildClientCode("1K", tc.prefix)
			if tc.want == "" {
				require.ErrorContains(t, err, organization.ReasonClientPrefixInvalid)
				return
			}
			require.NoError(t, err)
			require.Equal(t, tc.want, got)
		})
	}
}

func TestBuildProjectCode(t *testing.T) {
	got, err := organization.BuildProjectCode("1K-IBMI", organization.BusinessLineSecurity)
	require.NoError(t, err)
	require.Equal(t, "1K-IBMI-S", got)

	got, err = organization.BuildProjectCode("1K-IBMI", organization.BusinessLineCleaning)
	require.NoError(t, err)
	require.Equal(t, "1K-IBMI-C", got)

	_, err = organization.BuildProjectCode("1K-IBMI", "janitor")
	require.Error(t, err)
}

func TestBuildTeamCode(t *testing.T) {
	require.Equal(t, "1K-IBMI-S1", organization.BuildTeamCode("1K-IBMI-S", 1))
	require.Equal(t, "1K-IBMI-S10", organization.BuildTeamCode("1K-IBMI-S", 10))
}

func TestRegionOfCode(t *testing.T) {
	for code, want := range map[string]string{
		"OS1":        "",
		"1K":         "OS1",
		"1K-IBMI":    "OS1",
		"1K-IBMI-S1": "OS1",
		"12P-ABC-C3": "OS12",
		"":           "",
		"K1":         "",
	} {
		require.Equal(t, want, organization.RegionOfCode(code), code)
	}
}
