package organization

import (
	"regexp"
	"strconv"
	"strings"

	"siapp/pkg/access"
)

// ⚠ D19. Every row of the hierarchy is identified by a code that contains its parent's code,
// and the server builds it from the inputs. A code is never edited after creation: a change
// would have to rename every child, and every past attendance and report row that names it.
// This file is the only place a code is built or taken apart.
//
//	region   OS1                 OS{n}
//	branch   1K                  {n}{K|I|P}, n = its region's number
//	client   1K-IBMI             {branch}-{company_prefix}
//	project  1K-IBMI-S           {client}-{S|C}
//	team     1K-IBMI-S1          {project}{n}, n from 1 per project

// Business lines of a project (DL-20). The line decides the code's suffix.
const (
	BusinessLineSecurity = "security"
	BusinessLineCleaning = "cleaning"
)

var (
	branchCodePattern   = regexp.MustCompile(`^([1-9][0-9]*)([A-Z])$`)
	clientPrefixPattern = regexp.MustCompile(`^[A-Z0-9]{2,10}$`)
)

// companyLetters are the employing PTs of the group: K = PT Kharisma Potensia Indonesia (KPI),
// I = PT Insan Daya Sentosa (IDS), P = PT Potensial Cipta Daya (PCD).
const companyLetters = "KIP"

// RegionOfCode returns the koordinator wilayah a code sits under: OS plus the number the
// code starts with (1K-IBMI-S1 is under OS1). It is "" for a code that does not start with a
// number. An admin's scope is checked against it, so a row is reached through its region
// without a join. The rule itself lives in access.RegionOfCode, because access reads it too
// and cannot import this package; this is the same function under the name this file's callers use.
func RegionOfCode(code string) string { return access.RegionOfCode(code) }

// BuildBranchCode returns the code of a kantor cabang: the region's number and the company
// letter, so OS1 + K is 1K. The letter must be K, I or P (BRANCH_LETTER_INVALID).
func BuildBranchCode(regionCode, letter string) (string, error) {
	if !regionCodePattern.MatchString(regionCode) {
		return "", validationError("region_code", "Harus berformat OS diikuti angka tanpa nol di depan, misalnya OS1.")
	}
	if len(letter) != 1 || !strings.Contains(companyLetters, letter) {
		return "", ErrBranchLetterInvalid
	}
	return strings.TrimPrefix(regionCode, "OS") + letter, nil
}

// ValidateBranchCode checks a branch code that was not built here (the master data import
// brings its own): the letter must be K, I or P (BRANCH_LETTER_INVALID), and the number must
// be the region's (BRANCH_REGION_MISMATCH: 2K does not sit under OS1).
func ValidateBranchCode(regionCode, code string) error {
	m := branchCodePattern.FindStringSubmatch(code)
	if m == nil || !strings.Contains(companyLetters, m[2]) {
		return ErrBranchLetterInvalid
	}
	if "OS"+m[1] != regionCode {
		return ErrBranchRegionMismatch
	}
	return nil
}

// BuildClientCode returns the code of a klien: its branch's code, a dash and the company
// prefix, so 1K + IBMI is 1K-IBMI. The prefix is 2 to 10 capital letters or digits
// (CLIENT_PREFIX_INVALID).
func BuildClientCode(branchCode, prefix string) (string, error) {
	if !clientPrefixPattern.MatchString(prefix) {
		return "", ErrClientPrefixInvalid
	}
	return branchCode + "-" + prefix, nil
}

// BuildProjectCode returns the code of a proyek: its client's code and S for security or C
// for cleaning. A client therefore has at most one project per business line.
func BuildProjectCode(clientCode, businessLine string) (string, error) {
	switch businessLine {
	case BusinessLineSecurity:
		return clientCode + "-S", nil
	case BusinessLineCleaning:
		return clientCode + "-C", nil
	}
	return "", validationError("business_line", "Harus security atau cleaning.")
}

// BuildTeamCode returns the code of a regu: its project's code and the team's number.
func BuildTeamCode(projectCode string, number int) string {
	return projectCode + strconv.Itoa(number)
}
