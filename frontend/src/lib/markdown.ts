import DOMPurify, { type Config } from 'dompurify';
import { marked } from 'marked';

/** The shared markdown allowlist (dashboard text cards — the only
 * markdown consumer today). Deliberately strict: no attributes other
 * than a[href], no data: URIs (no data: images), no javascript:, no
 * protocol-relative (//host) or bare-relative (host/path) targets —
 * only http(s), same-document relative paths, and in-page fragments.
 * marked produces the HTML; DOMPurify strips anything outside the
 * allowlist before it reaches @html. */
const SANITIZE_CONFIG: Config = {
	ALLOWED_TAGS: [
		'h1',
		'h2',
		'h3',
		'h4',
		'h5',
		'h6',
		'p',
		'br',
		'strong',
		'em',
		'ul',
		'ol',
		'li',
		'code',
		'pre',
		'a',
		'blockquote',
		'hr',
		'table',
		'thead',
		'tbody',
		'tr',
		'th',
		'td'
	],
	ALLOWED_ATTR: ['href'],
	ALLOWED_URI_REGEXP: /^(?:https?:\/\/|\/|\.\/|\.\.\/|#)/i,
	ALLOW_DATA_ATTR: false
};

/** Renders markdown to sanitized HTML for @html: marked → DOMPurify
 * with the shared allowlist above. User-authored text is untrusted —
 * never skip the sanitize. Runs on each render of a markdown card
 * (cheap for note-sized text — same stance as parseState in the
 * explore cards). */
export function renderMarkdown(src: string): string {
	return DOMPurify.sanitize(marked.parse(src, { async: false }) as string, SANITIZE_CONFIG);
}
