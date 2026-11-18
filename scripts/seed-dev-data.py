#!/usr/bin/env python3
"""Seed the dev stack with realistic data for manual testing (round 19).

Creates a handful of extra accounts and categories (idempotent — existing
names are reused, never duplicated) and a few hundred transactions spread
over 2026-03..2026-11, mixing recurring income (payroll, quarterly
interest, freelance) with everyday spending across categories, including
some uncategorized rows, some notes, and a few review-pending rows.

All generated rows are deterministic (fixed RNG seed), so re-running is
safe: rows whose dedup key (account, date_posted, amount, description)
already exists are rejected with 409 and counted as skips.

Run (dev stack must be up — `make dev`):
    scripts/seed-dev-data.py

Env (same conventions as scripts/verify.sh authn):
    FTRK_API_URL              default http://localhost:18080
    FTRK_KC_URL               default http://localhost:18081
    FTRK_REALM                default finance
    FTRK_CLIENT_ID            default finance-spa
    FTRK_AUDIENCE             default finance-api
    FTRK_TEST_USER            default test
    FTRK_TEST_USER_PASSWORD   default test1234
    FTRK_SEED_TX_COUNT        default 280 (transactions to attempt)
"""

import json
import os
import random
import sys
import urllib.error
import urllib.parse
import urllib.request
from datetime import date, timedelta

API = os.environ.get("FTRK_API_URL", "http://localhost:18080") + "/api/v1"
KC = os.environ.get("FTRK_KC_URL", "http://localhost:18081")
REALM = os.environ.get("FTRK_REALM", "finance")
CLIENT_ID = os.environ.get("FTRK_CLIENT_ID", "finance-spa")
AUDIENCE = os.environ.get("FTRK_AUDIENCE", "finance-api")
USERNAME = os.environ.get("FTRK_TEST_USER", "test")
PASSWORD = os.environ.get("FTRK_TEST_USER_PASSWORD", "test1234")
TX_TARGET = int(os.environ.get("FTRK_SEED_TX_COUNT", "280"))

START = date(2026, 3, 1)
END = date(2026, 11, 10)  # "today" for the dev data set
RNG = random.Random(20261110)

# (name, type, subtype) — type values are the accounts.type vocabulary.
WANT_ACCOUNTS = [
    ("Everyday Checking", "bank", "chequing"),
    ("High-Yield Savings", "bank", "savings"),
    ("Store Card", "credit_card", "cashback"),
    ("Cash Wallet", "cash", None),
    ("TFSA", "investment", None),
]

WANT_CATEGORIES = ["Salary", "Interest", "Freelance", "Subscriptions", "Health", "Home"]

# category name -> (merchant descriptions, amount low, amount high)
SPENDING_POOLS = [
    ("Groceries", ["Sobeys", "Metro", "Loblaws", "Costco", "No Frills"], 25, 180),
    ("Dining", ["Tim Hortons", "Subway", "Pizza Port", "Sushiro", "Chipotle", "Starbucks"], 4, 90),
    ("Transport", ["Shell Gas", "Petro-Canada", "Parkade", "Metro Transit", "Uber"], 12, 90),
    ("Shopping", ["Best Buy", "Walmart", "Indigo", "Canadian Tire", "Amazon"], 10, 300),
    ("Entertainment", ["Steam", "Cineplex", "Spotify", "Ticketmaster"], 5, 120),
    ("Utilities", ["Enbridge Gas", "Hydro One", "Rogers", "Bell", "City Water"], 40, 220),
    ("Subscriptions", ["Netflix", "Amazon Prime", "iCloud", "Duolingo", "Adobe"], 5, 22),
    ("Travel", ["Air Canada", "Airbnb", "Marriott", "Greyhound"], 60, 900),
    ("Outdoor Gear", ["Canada Goose", "REI", "Decathlon", "Backcountry"], 30, 400),
    (None, ["DEBIT PURCHASE", "CASH W/DR", "POS TERMINAL"], 5, 80),  # uncategorized
]
SPENDING_WEIGHTS = [20, 20, 12, 12, 10, 6, 8, 5, 4, 3]

NOTES = [
    "gas top-up",
    "split with alex",
    "work laptop",
    "gym membership",
    "birthday gift",
    "weekly shop",
    "car maintenance",
    "coffee with the team",
]

FREELANCE_DESCS = ["CLIENT INVOICE #1042", "FIVERR PAYOUT", "CONTRACT WORK INVOICE"]


def http(method: str, path: str, body=None, token: str = ""):
    """One API call. Returns (status, parsed_json_or_text)."""
    url = API + path
    data = json.dumps(body).encode() if body is not None else None
    req = urllib.request.Request(url, data=data, method=method)
    req.add_header("Content-Type", "application/json")
    if token:
        req.add_header("Authorization", f"Bearer {token}")
    try:
        with urllib.request.urlopen(req) as r:
            raw = r.read().decode()
            return r.status, json.loads(raw) if raw else None
    except urllib.error.HTTPError as e:
        raw = e.read().decode()
        try:
            return e.code, json.loads(raw)
        except json.JSONDecodeError:
            return e.code, raw


def login() -> str:
    form = urllib.parse.urlencode(
        {
            "grant_type": "password",
            "username": USERNAME,
            "password": PASSWORD,
            "client_id": CLIENT_ID,
            "audience": AUDIENCE,
            "scope": "openid profile email",
        }
    ).encode()
    req = urllib.request.Request(
        f"{KC}/realms/{REALM}/protocol/openid-connect/token",
        data=form,
        method="POST",
        headers={"Content-Type": "application/x-www-form-urlencoded"},
    )
    with urllib.request.urlopen(req) as r:
        return json.loads(r.read().decode())["access_token"]


def ensure_accounts(token: str) -> dict:
    """Return name -> id for all accounts (existing + newly created)."""
    code, rows = http("GET", "/accounts", token=token)
    if code != 200:
        sys.exit(f"GET /accounts -> {code}: {rows}")
    by_name = {a["name"]: a["id"] for a in rows}
    for name, atype, subtype in WANT_ACCOUNTS:
        if name in by_name:
            continue
        body = {"name": name, "type": atype, "subtype": subtype, "currency": "CAD"}
        code, row = http("POST", "/accounts", body, token)
        if code not in (200, 201):
            sys.exit(f"POST /accounts {name!r} -> {code}: {row}")
        by_name[name] = row["id"]
        print(f"  + account {name!r} ({atype}/{subtype or '—'})")
    return by_name


def ensure_categories(token: str) -> dict:
    """Return name -> id for all categories (existing + newly created)."""
    code, rows = http("GET", "/categories", token=token)
    if code != 200:
        sys.exit(f"GET /categories -> {code}: {rows}")
    by_name = {c["name"]: c["id"] for c in rows}
    for name in WANT_CATEGORIES:
        if name in by_name:
            continue
        code, row = http("POST", "/categories", {"name": name}, token)
        if code not in (200, 201):
            sys.exit(f"POST /categories {name!r} -> {code}: {row}")
        by_name[name] = row["id"]
        print(f"  + category {name!r}")
    return by_name


def random_day() -> date:
    return START + timedelta(days=RNG.randrange((END - START).days + 1))


def gen_income(cat_ids: dict, acct_ids: dict) -> list:
    """Recurring income rows: biweekly payroll, quarterly interest, freelance."""
    rows = []
    checking = acct_ids["Everyday Checking"]
    savings = acct_ids["High-Yield Savings"]
    d = date(2026, 3, 6)
    while d <= END:
        rows.append(dict(
            account_id=checking,
            date_executed=d.isoformat(),
            date_posted=d.isoformat(),
            amount=f"+{2200 + RNG.randrange(400)}.{RNG.randrange(100):02d}",
            category_id=cat_ids["Salary"],
            description="ACME CORP PAYROLL E DIRECT DEP",
            notes=None,
            review_pending=False,
        ))
        d += timedelta(days=14)
    for d in (date(2026, 3, 31), date(2026, 6, 30), date(2026, 9, 30)):
        rows.append(dict(
            account_id=savings,
            date_executed=None,
            date_posted=d.isoformat(),
            amount=f"+{RNG.uniform(12, 40):.2f}",
            category_id=cat_ids["Interest"],
            description="INTEREST PAYMENT",
            notes="quarterly interest",
            review_pending=False,
        ))
    for _ in range(8):
        d = random_day()
        rows.append(dict(
            account_id=checking,
            date_executed=None,
            date_posted=d.isoformat(),
            amount=f"+{RNG.uniform(150, 600):.2f}",
            category_id=cat_ids["Freelance"],
            description=RNG.choice(FREELANCE_DESCS),
            notes=None,
            review_pending=False,
        ))
    return rows


def gen_spending(cat_ids: dict, acct_ids: dict, target: int) -> list:
    """Everyday spending rows, deterministic; keys unique within the batch."""
    spenders = ["Store Card", "Store Card", "Store Card", "Everyday Checking",
                "Everyday Checking", "Cash Wallet"]
    rows = []
    seen = set()
    while len(rows) < target:
        name, descs, lo, hi = RNG.choices(SPENDING_POOLS, weights=SPENDING_WEIGHTS)[0]
        acct = acct_ids[RNG.choice(spenders)]
        d = random_day()
        desc = RNG.choice(descs)
        if RNG.random() < 0.3:
            desc += f" #{RNG.randrange(1000):04d}"
        amt = f"-{RNG.uniform(lo, hi):.2f}"
        key = (acct, d.isoformat(), amt, desc)
        if key in seen:
            continue
        seen.add(key)
        notes = RNG.choice(NOTES) if RNG.random() < 0.15 else None
        executed = d.isoformat() if RNG.random() < 0.7 else (d - timedelta(days=1)).isoformat()
        if RNG.random() < 0.1:
            executed = None
        rows.append(dict(
            account_id=acct,
            date_executed=executed,
            date_posted=d.isoformat(),
            amount=amt,
            category_id=cat_ids[name] if name else None,
            description=desc,
            notes=notes,
            review_pending=False,
        ))
    # A few review-pending rows so the review queue has something to show.
    for row in RNG.sample(rows, 4):
        row["review_pending"] = True
    return rows


def main() -> None:
    print(f"Seeding {API} (target: {TX_TARGET} transactions)")
    token = login()
    code, me = http("GET", "/me", token=token)
    if code != 200:
        sys.exit(f"GET /me -> {code}: {me} (is the token valid?)")
    print(f"  authenticated as {me.get('email') or me.get('sub')}")

    print("Accounts:")
    acct_ids = ensure_accounts(token)
    print("Categories:")
    cat_ids = ensure_categories(token)

    income = gen_income(cat_ids, acct_ids)
    spending = gen_spending(cat_ids, acct_ids, TX_TARGET - len(income))
    rows = income + spending
    rows.sort(key=lambda r: r["date_posted"])

    created = skipped = 0
    for r in rows:
        code, body = http("POST", "/transactions", r, token)
        if code in (200, 201):
            created += 1
        elif code == 409:
            skipped += 1  # dedup key already present — re-run
        else:
            sys.exit(f"POST /transactions -> {code}: {body} (row: {r})")
    print(f"  transactions: {created} created, {skipped} skipped (dedup)")

    # Total row count, paged (list caps limit at 100).
    total = 0
    offset = 0
    while True:
        code, page = http("GET", f"/transactions?limit=100&offset={offset}", token=token)
        if code != 200:
            sys.exit(f"GET /transactions -> {code}: {page}")
        total += len(page)
        if len(page) < 100:
            break
        offset += 100
    print(f"  total transactions now: {total}")
    print("Done.")


if __name__ == "__main__":
    main()
