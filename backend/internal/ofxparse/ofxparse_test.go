package ofxparse

import (
	"errors"
	"strings"
	"testing"
)

func mustParseTrns(t *testing.T, in string) []Transaction {
	t.Helper()
	trns, err := Parse(strings.NewReader(in))
	if err != nil {
		t.Fatalf("Parse(%q) error: %v", in, err)
	}
	return trns
}

func wantTrn(t *testing.T, i int, got Transaction, date, amt, name, ref string) {
	t.Helper()
	if got.DatePosted != date || got.Amount != amt ||
		got.Name != name || got.RefNum != ref {
		t.Errorf("trn[%d] = {%q %q %q %q}, want {%q %q %q %q}",
			i, got.DatePosted, got.Amount, got.Name, got.RefNum,
			date, amt, name, ref)
	}
}

// qfxBMO is a BMO/RBC-style QFX export: the <STMTTRN> tag names BOTH
// the wrapping section (DTSTART/DTEND/CURRCODE/BALANCE) and each
// record (closed by "^"); ACCTINQ carries an "STMTTRN:" request tag
// that is not a section opener.
const qfxBMO = `OFX110
File/Version OFX 1.1.1
!
<ENVIRON
<SERVER
<RETTIME:20260131235959
</ENVIRON
<SIGNON
<DTSERVER:20260131
<ORG:BANKOFMONTREAL
<STATUS:0[0
</SIGNON
<BANKMSGSRSV1
<SONRS
<STATUS:0[0
<SESSION:BMO123
</SONRS
<ACCTINQ
<DTSTART:20251001
<DTEND:20251231
<STMTTRN:1
<ACCTID:1234567890
<ACCTTYPE:CHECKING
<ACCTKEY:1
</ACCTINQ
<STMTTRN
<DTSTART:20251001000000
<DTEND:20251231235959[-5]
<CURRCODE:CAD
<BALANCE:-1234.56
<STMTTRN
<TRNAMT:-45.23
<DTPOSTED:20251015
<NAME:GROCERY STORE #123
<REFNUM:100234
^
<STMTTRN
<TRNAMT:2500.00
<DTPOSTED:20251101
<NAME:PAYROLL DIRECT DEPOSIT
^
<STMTTRN
<TRNAMT:-12.99
<DTPOSTED:20251120123045[-5]
<NAME:STREAMING SERVICE
<CHECKNUM:118
^
<STMTTRN
<TRNAMT:-88.00
<DTPOSTED:20251210
<NAME:CAR INSURANCE PREMIUM
<REFNUM:2200
^
</STMTTRN
<CLOSINGBATCH
</CLOSINGBATCH
</BANKMSGSRSV1
`

func TestParseQFXBMOStyle(t *testing.T) {
	trns := mustParseTrns(t, qfxBMO)
	if len(trns) != 4 {
		t.Fatalf("len(trns) = %d, want 4 (section record or envelope junk leaked): %+v", len(trns), trns)
	}
	wantTrn(t, 0, trns[0], "20251015", "-45.23", "GROCERY STORE #123", "100234")
	wantTrn(t, 1, trns[1], "20251101", "2500.00", "PAYROLL DIRECT DEPOSIT", "")
	wantTrn(t, 2, trns[2], "20251120123045[-5]", "-12.99", "STREAMING SERVICE", "118") // CHECKNUM alias
	wantTrn(t, 3, trns[3], "20251210", "-88.00", "CAR INSURANCE PREMIUM", "2200")

	// A UTF-8 BOM in front of the file must be harmless.
	trns = mustParseTrns(t, "\uFEFF"+qfxBMO)
	if len(trns) != 4 || trns[0].Name != "GROCERY STORE #123" {
		t.Fatalf("BOM variant = %+v, want the same 4 records", trns)
	}
}

// ofxBankTransList is spec-conformant OFX: records live in
// <BANKTRANLIST>, each record is closed by </STMTTRN> (no "^").
const ofxBankTransList = `OFX100
%
!
<ENVIRON
<VERSION:102
</ENVIRON
<SESSION
<SONRQ
<ORG:RBC
<USERID:alice
</SONRQ
</SESSION
<BANKMSGSRSV1
<STMTTRNRS
<TRNUID:0
<STATUS:0[0
</STMTTRNRS
<BANKTRANLIST
<DTSTART:20251001
<DTEND:20251231
<STMTTRN
<TRNAMT:-55.55
<DTPOSTED:20251005123000[-5]
<FITID:abc123
<NAME:CAFÉ ~U00E9 ROASTERY
</STMTTRN
<STMTTRN
<TRNAMT:100.00
<DTPOSTED:20251020
<PAYEE:LANDLORD PAYMENT
<REFNUM:R1
</STMTTRN
</BANKTRANLIST
</BANKMSGSRSV1
`

func TestParseOFXBankTransList(t *testing.T) {
	trns := mustParseTrns(t, ofxBankTransList)
	if len(trns) != 2 {
		t.Fatalf("len(trns) = %d, want 2: %+v", len(trns), trns)
	}
	// DTPOSTED keeps its time + tz suffix raw; FITID is ignored;
	// ~U00E9 is decoded; PAYEE counts as NAME.
	wantTrn(t, 0, trns[0], "20251005123000[-5]", "-55.55", "CAFÉ é ROASTERY", "")
	wantTrn(t, 1, trns[1], "20251020", "100.00", "LANDLORD PAYMENT", "R1")
}

func TestUnescape(t *testing.T) {
	cases := []struct{ in, want string }{
		{"plain value", "plain value"},
		{"a:b", "a:b"},
		{"a\\:b", "a:b"},
		{"a\\;b", "a;b"},
		{"a\\,b", "a,b"},
		{"a\\^b", "a^b"},
		{"a\\~b", "a~b"},
		{"10\\,234.56", "10,234.56"},
		{"a\\\\b", "a\\b"},
		{"trailing \\", "trailing \\"},
		{"~U00E9", "\u00E9"},
		{"a~U00e9b", "a\u00e9b"},
		{"~U0041", "A"},
		{"~UZZZZ", "~UZZZZ"}, // bad hex: verbatim
		{"~U00E", "~U00E"},   // truncated: verbatim, no panic
		{"~U", "~U"},
		{"~", "~"},
		{"a~b", "a~b"},
		{"~N", "\n"},
		{"a~N b~Nc", "a\n b\nc"},
		{"~U00E9~N~U0041", "\u00E9\nA"},
	}
	for _, c := range cases {
		if got := unescape(c.in); got != c.want {
			t.Errorf("unescape(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestParseEmpty(t *testing.T) {
	cases := []struct {
		name string
		in   string
	}{
		{"empty input", ""},
		{"blank lines only", "\r\n \r\n\t\r\n"},
		{"envelope only", "OFX110\n<SIGNON\n<DTSERVER:20260131\n<ORG:BMO\n<STATUS:0[0\n</SIGNON\n"},
		{"ACCTINQ request tag is not a record", "OFX110\n<ACCTINQ\n<STMTTRN:1\n<ACCTID:123\n</ACCTINQ\n"},
		{"QFX section with no records", "<STMTTRN\n<DTSTART:20251001\n<BALANCE:0.00\n</STMTTRN\n"},
		{"record with only unknown tags", "<STMTTRN\n<STMTTRN\n<FITID:only-unknown-tags\n<DTSTART:20251001\n^\n</STMTTRN\n"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := Parse(strings.NewReader(c.in))
			if !errors.Is(err, ErrEmpty) {
				t.Errorf("Parse err = %v, want ErrEmpty", err)
			}
		})
	}
}

func TestParsePartialRecord(t *testing.T) {
	trns := mustParseTrns(t, "<STMTTRN\n<NAME:ONLY A NAME\n^\n")
	if len(trns) != 1 {
		t.Fatalf("len(trns) = %d, want 1: %+v", len(trns), trns)
	}
	wantTrn(t, 0, trns[0], "", "", "ONLY A NAME", "")

	// QFX nesting with a single amount-only record: the empty
	// section record is dropped, the partial one survives.
	trns = mustParseTrns(t, "<STMTTRN\n<STMTTRN\n<TRNAMT:42.00\n^\n</STMTTRN\n")
	if len(trns) != 1 {
		t.Fatalf("len(trns) = %d, want 1: %+v", len(trns), trns)
	}
	wantTrn(t, 0, trns[0], "", "42.00", "", "")
}

func TestParseCRLFAndUnterminated(t *testing.T) {
	// CRLF line endings throughout and a last record cut off before
	// its "^" (or closing tag): both records survive, in file order.
	in := "OFX110\r\n<BANKMSGSRSV1\r\n<STMTTRN\r\n<STMTTRN\r\n" +
		"<TRNAMT:-1.00\r\n<DTPOSTED:20250101\r\n<NAME:FIRST\r\n^\r\n" +
		"<STMTTRN\r\n<TRNAMT:-2.00\r\n<DTPOSTED:20250102\r\n<NAME:SECOND\r\n"
	trns := mustParseTrns(t, in)
	if len(trns) != 2 {
		t.Fatalf("len(trns) = %d, want 2: %+v", len(trns), trns)
	}
	wantTrn(t, 0, trns[0], "20250101", "-1.00", "FIRST", "")
	wantTrn(t, 1, trns[1], "20250102", "-2.00", "SECOND", "")
}
