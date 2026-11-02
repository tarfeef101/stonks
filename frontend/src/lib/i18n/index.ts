import { init, register } from 'svelte-i18n';

// Iteration 1 ships English only; later locales register here (and add a
// locale switcher in the AppShell).
register('en', () => import('./locales/en.json'));

init({
	fallbackLocale: 'en',
	initialLocale: 'en'
});

export { waitLocale } from 'svelte-i18n';
