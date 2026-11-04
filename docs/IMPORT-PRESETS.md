# Import presets — formats and sources (plan 4.4)

What each builtin import preset (`backend/internal/presets`) claims, how to
obtain the export from the FI's online banking, and where the format was
verified. Bank exports change without notice: **re-verify every URL below at
release (plan 11.3)** and update the preset if the layout moved. All sources
fetched **2026-11-04**.

App field vocabulary (`csvparse.Fields`, the row-column order parsed files
normalize to): `date_executed`, `date_posted`, `amount`, `description`,
`category`, `running_balance`.

## How presets work

Builtins are code, not DB rows: `presets.All()` seeds the import page
(plan 4.5); "save mapping as preset" writes per-user copies into
`import_schemas` (0001, `builtin = false`).

Matching (the file extension picks the format family):

| file           | claim order                                                              | fallback     |
| -------------- | ------------------------------------------------------------------------ | ------------ |
| `.csv`         | bank preset by filename token (exact), then header signature (ALL verified headers present) | `generic-csv`  |
| `.ofx`/`.qfx`  | bank preset by filename token                                            | `ofx-generic`|
| `.json`        | —                                                                        | `generic-json`|
| anything else  | —                                                                        | none (user picks format + preset, plan 4.5) |

Column resolution:

- CSV — per field: first matching candidate header (case-insensitive,
  trimmed), then a stored positional index (per-user presets, for
  headerless exports), else unmapped.
- JSON — per field: RFC 6901 pointer fallbacks, tried in order
  (`jsonparse`, plan 4.4).
- OFX/QFX — identity mapping: `ofxparse` (plan 4.3) already emits
  `date_posted`, `amount`, `name` (→ description), `refnum`.

## generic-csv

Any CSV. No preset mapping: 4.2's `Suggest` engine scores each column
against the field vocabulary and produces the pre-fill; the user confirms
before the import runs.

## generic-json

Any JSON document containing a transactions array. `jsonparse` locates the
array (document root, then `/transactions`, `/data`, `/items`, `/records`,
`/txns`, in that order) and resolves each field through alias pointers:

| field           | pointers tried |
| --------------- | -------------- |
| date_executed   | `/dateExecuted`, `/executedDate`, `/executionDate`, `/executeDate` |
| date_posted     | `/datePosted`, `/postedDate`, `/posted`, `/transactionDate`, `/txDate`, `/date` |
| amount          | `/amount`, `/transactionAmount`, `/txAmount`, `/value` |
| description     | `/description`, `/name`, `/memo`, `/narrative`, `/details`, `/payee` |
| category        | `/category`, `/categoryName`, `/category_name` |
| running_balance | `/runningBalance`, `/running_balance`, `/balance`, `/endBalance` |

## ofx-generic

OFX and QFX from any issuer. Plan 4.3's `ofxparse` handles both dialects
(QFX `<STMTTRN>` records; spec-conformant OFX `<BANKTRANLIST>`). The
bank-specific OFX presets below exist for identification and display;
parsing is identical to this one.

## TD Bank — CSV

- **Format:** "Spreadsheet (.CSV)" — a plain transaction list: date,
  description, amount, usually a running balance. Desktop web (EasyWeb):
  account → Account Activity → choose period → menu next to Download →
  "Spreadsheet (.CSV)".
- **Header row: UNVERIFIED.** No source publishes the exact TD CSV header
  row, so the preset claims by filename token only (`td` — `std` never
  claims it), maps by candidate-header aliases, and 4.2's `Suggest` fills
  anything the aliases miss:

  | field           | candidate headers |
  | --------------- | ----------------- |
  | date_executed   | execution date, execute date, executed date |
  | date_posted     | date, date posted, posted date, posting date |
  | amount          | amount, transaction amount, tx amount, amount (cad) |
  | description     | description, memo, details, transaction details |
  | running_balance | balance, running balance, end balance, ending balance |

- **Sources:** fininja.ca/banks/td-chequing/ · neontra.com/support/import-csv/td/

## Scotiabank — CSV

- **Format:** CSV download from the desktop web. Verified header row
  (all 7 columns, in order):
  `Filter, Date, Description, Sub-description, Type of Transaction, Amount, Balance`.
  `Filter` is mostly-empty statement metadata; `Sub-description` and
  `Type of Transaction` are deliberately unmapped (not transaction fields).
- **Preset:** claims by filename token (`scotia`, `scotiabank`) or by the
  full 7-column header signature (extra columns tolerated; every verified
  column must be present). Maps Date → `date_posted`, Description →
  `description`, Amount → `amount`, Balance → `running_balance`.
- **Sources:** official help "How do I download my transactions to
  Quickbooks?" (help.scotiabank.com/article/how-do-i-download-my-transactions-to-quickbooks) ·
  fininja.ca/banks/scotiabank/ (verified header row)

## BMO — OFX / QFX

- **Format:** ".ofx" (Quicken) from the Download menu above/right of the
  transactions table in web banking; one file per account.
- **Preset:** claims by filename token (`bmo`); parses with the
  ofx-generic parser (identity mapping).
- **Source:** neontra.com/support/import_qfx_ofx_pages/bmo/

## RBC — OFX / QFX

- **Format:** "Quicken" (`.qfx`) or "Money" (`.ofx`) from the Download
  control above the transactions table. CSV and QBO are also offered for
  history; the credit-card CSV is capped at ~3 months and has no running
  balance; the activity feed is not the eStatement. PDF eStatements are
  kept 7 years (dates `DD MMM YYYY`, separate debit/credit columns).
- **Preset:** claims by filename token (`rbc`, `royal`); parses with the
  ofx-generic parser. CSV exports (when a user has one) fall to
  `generic-csv` + Suggest.
- **Sources:** neontra.com/support/import_qfx_ofx_pages/rbc/ ·
  capyparse.com/banks/rbc

## Desjardins — OFX (the CSV has no header row)

- **Format:** AccèsD exports CSV (personal) or CSV / TXT / OFX (AccèsD
  Affaires). OFX is the builtin's format because the **business CSV has no
  header row** — 14 positional fields (caisse, folio, account type, date,
  sequence, description, cheque number, withdrawal, deposit, loan fields
  …, balance) — and there is no QBO at all. Windows: 62 days of detail by
  default, 12 months of download with online statements on; business
  credit cards: 4 months as OFX/CSV, one card per file. PDF relevés: 7
  years (caisse), FR/EN, separate debit/credit columns, French-style
  numbers (`1 234,56` → 1234.56).
- **Preset:** claims by filename token (`desjardins`); parses with the
  ofx-generic parser. The headerless CSV is still importable: save a
  per-user preset with positional `index` refs (that is what the `index`
  field of a saved `import_schemas.spec` is for), or use `generic-csv` +
  Suggest.
- **Sources:** official AccèsD Affaires guide PDF
  (desjardins.com/content/dam/pdf/en/business/accounts-treasury/accesd-affaires-guide.pdf,
  §§10.4 / 10.8) · capyparse.com/banks/desjardins (export behavior as of
  September 2026)

## CIBC — no builtin (PDF-centric)

CIBC has **no machine preset**. Recent activity exports as CSV or QFX from
"Download Transactions" (More → Download Transactions; account, date
range, format CSV/QFX/Excel), but the authoritative record — the monthly
eStatement, 7 years of archive — is **PDF only**, in no machine format.

- **Path:** export recent activity as CSV/QFX from online banking (the QFX
  parses via `ofx-generic`; the CSV via `generic-csv` + Suggest); for
  anything older — or when the statement itself is what you need — convert
  the PDF with a third-party converter (e.g. capyparse.com/banks/cibc,
  docuclipper) and import the resulting CSV via `generic-csv`.
- **Why no CSV preset:** the eStatement layout exists only in PDF, so a
  CSV preset would encode a third-party converter's output rather than a
  CIBC format — and converters drift independently of the bank.
- **Sources:** CIBC CA how-to "eStatements"
  (cibc.com/en/personal-banking/ways-to-bank/how-to/estatements.html) ·
  CIBC US how-to "Download transactions for Quicken or other software"
  (us.cibc.com/en/digital-banking/how-to.html) · CIBC FR eStatement guide
  PDF (cibc.com/content/dam/personal_banking/ways_to_bank/pdfs/how-toguide-cibc-estatements-fr.pdf) ·
  invoicedataextraction.com/blog/cibc-bank-statement-to-excel

## Drift and fixtures

- Bank export layouts change without notice. Everything above was verified
  2026-11-04; **re-verify every URL at release (plan 11.3)** and update the
  preset + this file if a layout moved.
- The parser test fixtures (plan 4.2 CSV, 4.3 OFX/QFX) are **synthetic
  style samples** — they exercise the parsers' dialect handling, not
  verified bank layouts. Only the Scotiabank header row above is a
  verified bank layout; the TD row is unverified by design (aliases +
  Suggest cover it).
