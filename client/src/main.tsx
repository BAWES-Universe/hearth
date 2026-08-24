import { render } from 'preact';
import { registerSW } from 'virtual:pwa-register';
import { App } from './App';
import { initAnalytics, track } from './analytics/analytics';
import './style.css';

registerSW({ immediate: true });

// Analytics v0: scrubbed product events to the self-hosted intake (batch +
// fire-and-forget; consent flag respected, see analytics.ts).
initAnalytics();
track('session_start', { client_version: '0.1.0', platform: 'web' });

render(<App />, document.getElementById('app')!);
