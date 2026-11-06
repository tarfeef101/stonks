// LLM credentials API (step 5.6): the caller's LLM provider
// credentials, at most one per provider (self_hosted, openai, custom).
// The API key is sealed with the caller's DEK on the server and is
// never read back — the wire carries only its mask ("" = the row has
// no key). Test-connection fires one trivial chat-completions call at
// the endpoint the form shows and reports the latency and the tokens
// the provider used.
//
// Success bodies are the raw values the backend returns (no envelope);
// non-2xx responses are the 3.1 envelope {error: {code, message}} and
// surface as ApiError (see client.ts).

import { api } from './client';

/** The providers the credentials table accepts (the 0003 vocabulary). */
export type LLMProvider = 'self_hosted' | 'openai' | 'custom';

/** One credential row (backend llmCredOut): the endpoint, the key's
 * mask ("" = the row has no key), and the row timestamps (RFC3339
 * UTC). The key itself is never returned. */
export interface LLMCredential {
	provider: LLMProvider;
	endpoint: string;
	api_key_masked: string;
	created_at: string;
	updated_at: string;
}

/** The test-connection result (backend llmCredTestOut): the round-trip
 * latency and the usage the provider reported (0 when it omits usage),
 * the model echo (absent when the provider does not send one). */
export interface LLMTestResult {
	latency_ms: number;
	tokens_in: number;
	tokens_out: number;
	model?: string;
}

/** The caller's credentials (GET /v1/llm-credentials), at most one per
 * provider, in provider order. An empty set is []. */
export function listLLMCredentials(): Promise<LLMCredential[]> {
	return api<LLMCredential[]>('/v1/llm-credentials');
}

/** Creates or replaces the caller's credential for provider (PUT
 * /v1/llm-credentials/{provider} — 200 + the stored row, key masked).
 * apiKey is null for "no key" (valid only for self_hosted — openai and
 * custom answer 400 without one). */
export function putLLMCredential(
	provider: LLMProvider,
	endpoint: string,
	apiKey: string | null
): Promise<LLMCredential> {
	return api<LLMCredential>(`/v1/llm-credentials/${provider}`, {
		method: 'PUT',
		body: { endpoint, api_key: apiKey }
	});
}

/** Removes the caller's credential for provider (DELETE
 * /v1/llm-credentials/{provider} — 204). A row the caller does not
 * have (including an unknown provider) answers 404. */
export function deleteLLMCredential(provider: LLMProvider): Promise<void> {
	return api<void>(`/v1/llm-credentials/${provider}`, { method: 'DELETE' });
}

/** Fires one trivial chat-completions call at the FORM values (POST
 * /v1/llm-credentials/test — 200 + the latency/token result). Validated
 * like the PUT (400 before any call — including openai/custom without
 * a key); a failed round trip (connection refused, non-2xx, timeout)
 * answers 400 with the provider's error (never the key). */
export function testLLMConnection(
	provider: LLMProvider,
	endpoint: string,
	apiKey: string | null
): Promise<LLMTestResult> {
	return api<LLMTestResult>('/v1/llm-credentials/test', {
		method: 'POST',
		body: { provider, endpoint, api_key: apiKey }
	});
}
