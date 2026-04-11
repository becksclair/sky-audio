import { test, expect } from '@playwright/test';
import { readFileSync } from 'node:fs';
import { fileURLToPath } from 'node:url';
import path from 'node:path';

const thisDir = path.dirname(fileURLToPath(import.meta.url));
const liveSTTBaseline = JSON.parse(
  readFileSync(
    path.resolve(thisDir, '../../gateway/e2e/testdata/sky_voice_test.openai-baseline.json'),
    'utf8',
  ),
) as {
  expected_transcript: string;
};

function normalizeTranscript(text: string): string {
  return text
    .toLowerCase()
    .replace(/[^a-z0-9]+/g, ' ')
    .replace(/\s+/g, ' ')
    .trim();
}

function getNodeEnv(name: string): string | undefined {
  return (
    globalThis as {
      process?: {
        env?: Record<string, string | undefined>;
      };
    }
  ).process?.env?.[name];
}

const liveWebRTCMode = getNodeEnv('PLAYWRIGHT_LIVE_WEBRTC') === '1';

test.describe('WebRTC Connect Flow (live gateway)', () => {
  test.skip(!liveWebRTCMode, 'Set PLAYWRIGHT_LIVE_WEBRTC=1 to enable live WebRTC integration test.');

  test('connects to live gateway and streams transcript text from fake mic input', async ({ page }) => {
    test.setTimeout(90000);

    const health = await page.request.get('/health').catch(() => null);
    test.skip(!health || !health.ok(), 'Gateway /health is unavailable for live WebRTC test.');

    await page.addInitScript((apiKey: string) => {
      const originalFetch = window.fetch.bind(window);
      window.fetch = (input: RequestInfo | URL, init?: RequestInit) => {
        const url = typeof input === 'string' ? input : input instanceof URL ? input.toString() : input.url;
        if (url.startsWith('/api/') || url.startsWith('/v1/')) {
          const headers = new Headers(init?.headers ?? (input instanceof Request ? input.headers : undefined));
          if (!headers.has('Authorization')) {
            headers.set('Authorization', `Bearer ${apiKey}`);
          }
          return originalFetch(input, {
            ...init,
            headers,
          });
        }
        return originalFetch(input, init);
      };
    }, 'playwright-live');

    await page.goto('/dashboard/');
    await expect(page.locator('h1')).toContainText('Sky Audio Playground');

    await page.locator('button:has-text("Connect WebRTC")').click();

    const statusLine = page.locator('p:has-text("Status:")');
    const transcriptFeed = page.locator('.htk-chat');

    await expect(page.locator('button:has-text("Disconnect")')).toBeVisible({ timeout: 45000 });
    await expect(statusLine).toContainText('connected', { timeout: 45000 });
    await expect(statusLine).toContainText('Session', { timeout: 45000 });
    await expect(transcriptFeed).toContainText('Connected. Speak into your microphone.', {
      timeout: 45000,
    });

    const expectedTranscript = normalizeTranscript(liveSTTBaseline.expected_transcript);
    await expect
      .poll(
        async () => normalizeTranscript((await transcriptFeed.textContent()) ?? ''),
        { timeout: 60000 },
      )
      .toContain(expectedTranscript);

    await page.locator('button:has-text("Disconnect")').click();
    await expect(page.locator('button:has-text("Connect WebRTC")')).toBeVisible();
    await expect(statusLine).toContainText('disconnected');
  });
});
