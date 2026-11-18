// Import-job API (step 4.5): upload a bank statement, analyze it, map
// its fields, preview the mapping live, and save the mapping as a preset.
//
// Success bodies are the raw values the backend returns (no envelope);
// non-2xx responses are the 3.1 envelope {error: {code, message}} and
// surface as ApiError (see client.ts).

import { api } from './client';

/** One import job (backend importOut). `result` and `mapping` are raw
 * JSON documents — null while unset (the mapping lands with 4.5, the
 * run result with 4.6). */
export interface ImportJob {
	id: string;
	account_id: string;
	file_name: string;
	status: string;
	result: unknown;
	mapping: unknown;
	error: string | null;
}

export type MappingKind = 'csv' | 'ofx' | 'json';

/** How one app field is located inside a file (backend fieldRef).
 * CSV: headers (candidate names) + index (0-based, -1 = unmapped).
 * JSON: RFC 6901 pointers tried in order. */
export interface FieldRef {
	headers?: string[];
	index?: number;
	pointers?: string[];
}

/** The mapping document (backend Mapping): the 6 canonical fields the
 * 4.6 runner maps every row into. OFX is always the identity
 * ({kind: 'ofx', map: {}} — the OFX fields are fixed). */
export interface ImportMapping {
	kind: MappingKind;
	array_pointers?: string[];
	map: Record<string, FieldRef>;
}

/** A preset row from GET /v1/imports/presets (backend presetOut): the
 * code builtins followed by the caller's saved presets. */
export interface ImportPreset {
	id: string;
	name: string;
	bank: string;
	builtin: boolean;
	kind: string;
	spec: unknown;
}

/** One csvparse.Column in the analyze response — the inferred kind is
 * the type badge in the mapping table. */
export interface AnalyzeColumn {
	index: number;
	heading: string;
	kind: string;
}

/** The analyze response (backend analyzeOut). `preset` is the matched
 * builtin/saved preset (null for JSON, which always suggests). */
export interface AnalyzeResult {
	kind: MappingKind;
	preset: { id: string; name: string } | null;
	headers?: string[];
	columns?: AnalyzeColumn[];
	suggested: ImportMapping;
	keys?: string[];
}

/** The preview response (backend previewOut): Columns and Rows are
 * aligned; Total is the whole-file mapped row count, not the page's. */
export interface PreviewResult {
	columns: string[];
	rows: string[][];
	total: number;
}

/** An account row (backend accountOut) — the upload picker's data. */
export interface Account {
	id: string;
	name: string;
	type: string;
	subtype: string | null;
	currency: string;
	default_schema_id: string | null;
}

/** A saved preset (backend schemaOut) — the 201 of "save as preset".
 * Customs only since 0018: no builtin flag (the code presets carry
 * theirs on the /imports/presets response). */
export interface ImportSchema {
	id: string;
	name: string;
	kind: string;
	spec: unknown;
}

/** The account list (3.1 endpoint, reused by the upload picker). */
export function listAccounts(): Promise<Account[]> {
	return api<Account[]>('/v1/accounts');
}

/** Uploads a statement file (multipart: file + account_id) and returns
 * the created job (201). */
export function uploadImport(accountId: string, file: File): Promise<ImportJob> {
	const form = new FormData();
	form.append('file', file);
	form.append('account_id', accountId);
	return api<ImportJob>('/v1/imports', { method: 'POST', body: form });
}

export function listImportPresets(): Promise<ImportPreset[]> {
	return api<ImportPreset[]>('/v1/imports/presets');
}

/** Analyzes a job. Pass a preset ID to resolve that preset instead of
 * the automatic filename/header match. */
export function analyzeImport(id: string, presetId?: string): Promise<AnalyzeResult> {
	const q = presetId ? `?preset=${encodeURIComponent(presetId)}` : '';
	return api<AnalyzeResult>(`/v1/imports/${id}/analyze${q}`);
}

export function previewImport(
	id: string,
	mapping: ImportMapping,
	limit: number
): Promise<PreviewResult> {
	return api<PreviewResult>(`/v1/imports/${id}/preview`, {
		method: 'POST',
		body: { mapping, limit }
	});
}

/** Stores the mapping on the job. The document goes out as a string so
 * the backend keeps exactly the bytes sent (it stores the body
 * verbatim). Returns the job (status 'mapping', mapping = sent bytes). */
export function setImportMapping(id: string, mapping: ImportMapping): Promise<ImportJob> {
	return api<ImportJob>(`/v1/imports/${id}/mapping`, {
		method: 'POST',
		body: JSON.stringify(mapping)
	});
}

/** "Save mapping as preset" (3.3 create): the document is stored
 * verbatim as the preset's spec. */
export function createImportSchema(
	name: string,
	kind: MappingKind,
	spec: ImportMapping
): Promise<ImportSchema> {
	return api<ImportSchema>('/v1/import-schemas', {
		method: 'POST',
		body: { name, kind, spec }
	});
}
