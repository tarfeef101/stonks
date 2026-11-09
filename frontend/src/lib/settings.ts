// Per-user settings client (step 1.4b) — typed access to the settings the
// backend derives from the users row (GET/PATCH /api/v1/settings, 1.4a).
// The server is the single source of truth for onboarding state:
// `onboarded_at` is stamped one-way when the onboarding form is submitted,
// and every onboarding gate in the frontend (callback redirect, UserChip
// prompt, onboarding-page state) reads from this store.
//
// Fail-open semantics: a settings load failure is treated as "unknown",
// never as "onboarding incomplete" — a flaky GET (backend restarting, 503
// while bootstrap is still provisioning) must not trap the user in an
// onboarding loop.

import { writable } from 'svelte/store';
import { api } from '#lib/api/client';

/** Mirrors the backend Settings JSON (backend/internal/server/settings.go). */
export interface Settings {
	email: string;
	display_name: string;
	language: string;
	default_currency: string;
	default_period: string;
	/** Appearance: 'light' | 'dark' | 'system' (follow the OS/browser
	 * preference). 'system' is the DB default (0011). */
	theme: string;
	notify_monthly: boolean;
	notify_quarterly: boolean;
	notify_annual: boolean;
	/** ISO-8601 timestamp, or null while onboarding is incomplete. */
	onboarded_at: string | null;
	/** Server-wide: whether transactional email is configured (step 8.1). */
	mail_enabled: boolean;
}

/**
 * Any subset of the settings fields. Omitted keys are untouched on the
 * server (every PATCH body field is a pointer). `onboarded` is the one-way
 * completion flag: `true` stamps `onboarded_at`, `false` is a 400.
 */
export type SettingsPatch = Partial<Omit<Settings, 'onboarded_at'>> & {
	onboarded?: boolean;
};

export const settings = writable<Settings | null>(null);
export const settingsLoading = writable(false);

/** Clears the current user's settings (sign-out) so the next session
 * never renders the previous user's data or onboarding state. */
export function clearSettings(): void {
	settings.set(null);
	settingsLoading.set(false);
}

/**
 * Load the signed-in user's settings into the store. Returns the settings,
 * or null on any error (no session, network, 401/503) — see the fail-open
 * note in the header.
 */
export async function loadSettings(): Promise<Settings | null> {
	settingsLoading.set(true);
	try {
		const st = await api<Settings>('/v1/settings');
		settings.set(st);
		return st;
	} catch {
		settings.set(null);
		return null;
	} finally {
		settingsLoading.set(false);
	}
}

/**
 * PATCH the settings. The backend answers with the full updated Settings
 * in one round trip; it is stored here so the onboarding gates see the new
 * state the moment the onboarding submit navigates away (no second GET).
 * Throws ApiError on failure — callers surface the envelope message.
 */
export async function updateSettings(patch: SettingsPatch): Promise<Settings> {
	const st = await api<Settings>('/v1/settings', { method: 'PATCH', body: patch });
	settings.set(st);
	return st;
}

/**
 * Curated ISO-4217 codes for the default-currency picker (CAD first — the
 * primary home market per DESIGN.md). The backend accepts any three-letter
 * code, so this is a convenience list, not a constraint.
 */
export const CURRENCIES: string[] = [
	'CAD',
	'USD',
	'EUR',
	'GBP',
	'AUD',
	'NZD',
	'CHF',
	'JPY',
	'CNY',
	'INR',
	'SGD',
	'HKD',
	'SEK',
	'NOK',
	'DKK',
	'PLN',
	'CZK',
	'HUF',
	'RON',
	'BGN',
	'MXN',
	'BRL',
	'ARS',
	'CLP',
	'ZAR',
	'KRW',
	'THB',
	'MYR',
	'IDR',
	'PHP',
	'VND',
	'TRY',
	'ILS',
	'AED',
	'SAR'
];

/**
 * The default transaction categories previewed on the onboarding page
 * ("adjust later" — DESIGN.md: sensible defaults that are able to be
 * toggled off). Step 2.5 seeds each user's user_categories from this same
 * list, so it stays the single source of the defaults (no drift between
 * the preview and what gets seeded).
 */
export const DEFAULT_CATEGORY_NAMES: string[] = [
	'Housing',
	'Utilities',
	'Groceries',
	'Transportation',
	'Dining out',
	'Health',
	'Insurance',
	'Entertainment',
	'Subscriptions',
	'Travel',
	'Savings',
	'Other'
];
