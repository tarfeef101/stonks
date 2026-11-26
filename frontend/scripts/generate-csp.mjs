#!/usr/bin/env node
// Emits the nginx security-header TEMPLATE (shipped as
// /etc/nginx/csp-template.conf) from the REAL built index.html — run
// in the Dockerfile after `npm run build`. The Content-Security-Policy
// hashes every inline <script> in the artifact: the theme pre-paint
// snippet (app.html) is stable across builds, but the SvelteKit
// bootstrap embeds a per-build version string, so the hashes MUST come
// from the artifact, never be hardcoded. Fails the build when no
// inline scripts are found (a CSP without the bootstrap hash would
// break the app on first paint).
//
// Template mode (round 64): the deploy-time values are NOT known at
// build time, so two markers remain in the output for the nginx
// entrypoint (nginx/entrypoint.sh) to render from env at container
// start:
//   __ISSUER_ORIGIN__   IdP origin (scheme + host[:port]) → frame-src
//                       (silent-renew iframe) and connect-src
//                       (browser-side OIDC discovery / token / userinfo)
//   __CONNECT_EXTRA__   ' https://bam.nr-data.net' when the deployment
//                       sets NR_LICENSE_KEY (NR browser agent on),
//                       empty otherwise — the marker sits DIRECTLY after
//                       'self', so an empty render yields a clean
//                       "connect-src 'self'" (no trailing space).
//
// Env:
//   CSP_INDEX          built index.html to hash (default build/index.html)
//   CSP_OUT            where to write the template (default csp.conf)
import { createHash } from 'node:crypto';
import { readFileSync, writeFileSync } from 'node:fs';

function fail(msg) {
	console.error(`generate-csp: ${msg}`);
	process.exit(1);
}

const index = process.env.CSP_INDEX ?? 'build/index.html';
const out = process.env.CSP_OUT ?? 'csp.conf';

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

const csp = [
	"default-src 'self'",
	`script-src 'self' ${hashes.join(' ')}`,
	// 'unsafe-inline' for styles: app.html uses a style="" attribute on
	// the body div, and Tailwind's output is <link>-based anyway.
	"style-src 'self' 'unsafe-inline'",
	"img-src 'self'",
	// __CONNECT_EXTRA__ is rendered at container start (see header).
	"connect-src 'self'__CONNECT_EXTRA__",
	"object-src 'none'",
	// 'self' (the silent-renew iframe lands on /auth/callback after the
	// issuer's 302) plus the issuer origin (the iframe's first
	// navigation is the IdP authorize endpoint).
	"frame-src 'self' __ISSUER_ORIGIN__",
	"frame-ancestors 'none'",
	"base-uri 'self'",
	"form-action 'self'"
].join('; ');

const lines = [
	'# Personal Finance Tracker — security headers (TEMPLATE).',
	'# AUTO-GENERATED per build by scripts/generate-csp.mjs from the built',
	'# index.html — the inline-script hashes change with EVERY build. Do',
	'# not edit by hand; change the generator and rebuild. The two',
	'# __MARKERS__ are rendered by the container entrypoint',
	'# (nginx/entrypoint.sh) from deploy-time env (OIDC_ISSUER,',
	'# NR_LICENSE_KEY) into /tmp/nginx/headers.conf at startup.',
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
console.log(`generate-csp: ${hashes.length} inline script hash(es) → ${out} (markers left for render time)`);
