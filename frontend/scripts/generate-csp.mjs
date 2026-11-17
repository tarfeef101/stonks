#!/usr/bin/env node
// Derives the nginx security-header include (shipped as
// /etc/nginx/headers.conf, L41) from the REAL built index.html — run
// in the Dockerfile after `npm run build`. The Content-Security-Policy
// hashes every inline <script> in the artifact: the theme pre-paint
// snippet (app.html) is stable across builds, but the SvelteKit
// bootstrap embeds a per-build version string, so the hashes MUST come
// from the artifact, never be hardcoded. Fails the build when no
// inline scripts are found (a CSP without the bootstrap hash would
// break the app on first paint) or when the issuer is not a URL.
//
// Env:
//   CSP_INDEX          built index.html to hash (default build/index.html)
//   CSP_OUT            where to write the include (default csp.conf)
//   CSP_ISSUER         IdP issuer URL — its origin goes into frame-src
//                      (default http://localhost:18081/realms/finance,
//                      i.e. the src/lib/auth/client.ts fallback)
//   CSP_CONNECT_EXTRA  extra connect-src origin (default none; the
//                      Dockerfile sets https://bam.nr-data.net when
//                      PUBLIC_NR_LICENSE_KEY is set — i.e. the NR
//                      browser agent is compiled into the bundle)
import { createHash } from 'node:crypto';
import { readFileSync, writeFileSync } from 'node:fs';

function fail(msg) {
	console.error(`generate-csp: ${msg}`);
	process.exit(1);
}

const index = process.env.CSP_INDEX ?? 'build/index.html';
const out = process.env.CSP_OUT ?? 'csp.conf';
const issuer = process.env.CSP_ISSUER ?? 'http://localhost:18081/realms/finance';
const connectExtra = process.env.CSP_CONNECT_EXTRA ?? '';

let html;
try {
	html = readFileSync(index, 'utf8');
} catch (e) {
	fail(`cannot read ${index}: ${e.message}`);
}

// Inline scripts = <script> tags WITHOUT a src attribute (external
// bundles are 'self'-covered; only inline bodies need a hash).
const INLINE_RE = /<script(?![^>]*\bsrc\s*=)[^>]*>([\s\S]*?)<\/script>/gi;
const hashes = [];
for (const m of html.matchAll(INLINE_RE)) {
	hashes.push('sha256-' + createHash('sha256').update(m[1], 'utf8').digest('base64'));
}
if (hashes.length === 0) {
	fail(`no inline <script> in ${index} — refusing to emit a CSP without the bootstrap hash`);
}

let issuerOrigin;
try {
	issuerOrigin = new URL(issuer).origin;
} catch {
	fail(`CSP_ISSUER is not a URL: ${issuer}`);
}

const connectSrc =
	"'self'" +
	(connectExtra.trim() === '' ? '' : ' ' + connectExtra.trim().split(/\s+/).join(' '));

const csp = [
	"default-src 'self'",
	`script-src 'self' ${hashes.join(' ')}`,
	// 'unsafe-inline' for styles: app.html uses a style="" attribute on
	// the body div, and Tailwind's output is <link>-based anyway.
	"style-src 'self' 'unsafe-inline'",
	"img-src 'self'",
	`connect-src ${connectSrc}`,
	"object-src 'none'",
	// 'self' (the silent-renew iframe lands on /auth/callback after the
	// issuer's 302) plus the issuer origin (the iframe's first
	// navigation is the IdP authorize endpoint).
	`frame-src 'self' ${issuerOrigin}`,
	"frame-ancestors 'none'",
	"base-uri 'self'",
	"form-action 'self'"
].join('; ');

const lines = [
	'# Personal Finance Tracker — security headers (L41).',
	'# AUTO-GENERATED per build by scripts/generate-csp.mjs from the built',
	'# index.html — the inline-script hashes change with EVERY build. Do',
	'# not edit by hand; change the generator and rebuild.',
	'# Included at server level AND repeated inside every location that',
	'# sets its own add_header (nginx add_header inheritance rule).',
	'',
	`add_header Content-Security-Policy "${csp}" always;`,
	'add_header X-Content-Type-Options "nosniff" always;',
	'add_header X-Frame-Options "DENY" always;',
	'add_header Referrer-Policy "no-referrer" always;',
	'add_header Permissions-Policy "camera=(), geolocation=(), microphone=(), payment=()" always;',
	''
];
writeFileSync(out, lines.join('\n'));
console.log(`generate-csp: ${hashes.length} inline script hash(es) → ${out}`);
console.log(`generate-csp: frame-src 'self' ${issuerOrigin}`);
