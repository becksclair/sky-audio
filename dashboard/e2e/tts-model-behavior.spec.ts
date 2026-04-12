import { test, expect, type Page, type Route } from '@playwright/test';

let kyutaiCatalogAvailable = false;
const SILENT_WAV =
  'RIFF\x1e\x00\x00\x00WAVEfmt \x10\x00\x00\x00\x01\x00\x01\x00@\x1f\x00\x00@\x1f\x00\x00\x01\x00\x10\x00data\x02\x00\x00\x00\x00\x00';

async function fulfillSilentWav(route: Route): Promise<void> {
  await route.fulfill({
    status: 200,
    contentType: 'audio/wav',
    body: SILENT_WAV,
  });
}

async function requireKyutai(page: Page): Promise<void> {
  test.skip(!kyutaiCatalogAvailable, 'Kyutai engine is not available in this environment.');
  await page.locator('#tts-engine').selectOption('kyutai');
}

test.describe('TTS Model Behavior - Emotion Tag and Synthesis Options', () => {
  test.beforeAll(async ({ request }) => {
    kyutaiCatalogAvailable = false;
    try {
      const resp = await request.get('/api/tts/voices');
      if (!resp.ok()) {
        return;
      }
      const payload = (await resp.json()) as Record<string, unknown>;
      kyutaiCatalogAvailable = Boolean(payload.kyutai);
    } catch {
      kyutaiCatalogAvailable = false;
    }
  });

  test.beforeEach(async ({ page }) => {
    await page.goto('/');
    await expect(page.locator('h1')).toContainText('Sky Audio Playground');
  });

  test.describe('Emotion Tag Prepending', () => {
    test('[kyutai-only] request includes selected emotion tag', async ({ page }) => {
      await requireKyutai(page);
      await page.locator('#tts-input').fill('Hello world');
      await page.locator('#tts-emotion').selectOption('amusement');

      await page.route('/api/tts/synthesize', async (route) => {
        const postData = route.request().postDataJSON();
        expect(postData.text).toBe('[emotion:amusement] Hello world');
        await fulfillSilentWav(route);
      });

      await page.locator('button:has-text("Synthesize")').click();
    });

    test('[kyutai-only] tagged request preserves original text content', async ({ page }) => {
      await requireKyutai(page);
      await page.locator('#tts-input').fill('Hello world');
      await page.locator('#tts-emotion').selectOption('desire');

      await page.route('/api/tts/synthesize', async (route) => {
        const postData = route.request().postDataJSON();
        expect(postData.text).toBe('[emotion:desire] Hello world');
        await fulfillSilentWav(route);
      });

      await page.locator('button:has-text("Synthesize")').click();
    });

    test('[kyutai-only] no emotion tag in request when not-set is selected', async ({ page }) => {
      await requireKyutai(page);
      await page.locator('#tts-input').fill('Hello world');
      await page.locator('#tts-emotion').selectOption('not-set');

      await page.route('/api/tts/synthesize', async (route) => {
        const postData = route.request().postDataJSON();
        expect(postData.text).toBe('Hello world');
        expect(postData.text).not.toContain('[emotion:');
        await fulfillSilentWav(route);
      });

      await page.locator('button:has-text("Synthesize")').click();
    });

    test('[kyutai-only] all 9 emotions can be selected and sent correctly', async ({ page }) => {
      await requireKyutai(page);

      const emotions = [
        'neutral',
        'amusement',
        'interest',
        'desire',
        'adoration',
        'contentment',
        'serenity',
        'pride',
        'whisper',
      ];

      await page.route('/api/tts/synthesize', async (route) => {
        await fulfillSilentWav(route);
      });

      await page.locator('#tts-input').fill('Test text');
      const synthButton = page.locator('button:has-text("Synthesize")');

      for (const emotion of emotions) {
        await page.locator('#tts-emotion').selectOption(emotion);
        await expect(synthButton).toBeEnabled();

        const reqPromise = page.waitForRequest('**/api/tts/synthesize');
        await synthButton.click();
        const req = await reqPromise;
        const postData = req.postDataJSON();
        expect(postData.text).toBe(`[emotion:${emotion}] Test text`);

        await expect(synthButton).toBeEnabled();
      }
    });
  });

  test.describe('Synthesis Options in Request', () => {
    test('[kyutai-only] kyutai request includes default synthesis options', async ({ page }) => {
      await requireKyutai(page);
      await page.locator('#tts-input').fill('Hello world');

      await page.route('/api/tts/synthesize', async (route) => {
        const postData = route.request().postDataJSON();
        expect(postData.temperature).toBe(0.75);
        expect(postData.cfg_coef).toBe(1.5);
        expect(postData.padding_between).toBe(0);
        await fulfillSilentWav(route);
      });

      await page.locator('button:has-text("Synthesize")').click();
    });

    test('[kyutai-only] kyutai request includes custom synthesis options', async ({ page }) => {
      await requireKyutai(page);
      await page.locator('#tts-input').fill('Hello world');

      await page.locator('#tts-temperature').evaluate((el: HTMLInputElement) => {
        el.value = '1.25';
        el.dispatchEvent(new Event('input', { bubbles: true }));
      });
      await page.locator('#tts-cfg-coef').evaluate((el: HTMLInputElement) => {
        el.value = '2.0';
        el.dispatchEvent(new Event('input', { bubbles: true }));
      });
      await page.locator('#tts-padding').evaluate((el: HTMLInputElement) => {
        el.value = '3';
        el.dispatchEvent(new Event('input', { bubbles: true }));
      });

      await page.route('/api/tts/synthesize', async (route) => {
        const postData = route.request().postDataJSON();
        expect(postData.temperature).toBe(1.25);
        expect(postData.cfg_coef).toBe(2);
        expect(postData.padding_between).toBe(3);
        await fulfillSilentWav(route);
      });

      await page.locator('button:has-text("Synthesize")').click();
    });

    test('pocket request does not include synthesis options', async ({ page }) => {
      await page.locator('#tts-engine').selectOption('pocket');
      await page.locator('#tts-input').fill('Hello world');

      await page.route('/api/tts/synthesize', async (route) => {
        const postData = route.request().postDataJSON();
        expect(postData.temperature).toBeUndefined();
        expect(postData.cfg_coef).toBeUndefined();
        expect(postData.padding_between).toBeUndefined();
        await fulfillSilentWav(route);
      });

      await page.locator('button:has-text("Synthesize")').click();
    });

    test('[kyutai-only] request includes both emotion tag and synthesis options', async ({ page }) => {
      await requireKyutai(page);
      await page.locator('#tts-input').fill('Hello world');
      await page.locator('#tts-emotion').selectOption('pride');

      await page.locator('#tts-temperature').evaluate((el: HTMLInputElement) => {
        el.value = '0.95';
        el.dispatchEvent(new Event('input', { bubbles: true }));
      });

      await page.route('/api/tts/synthesize', async (route) => {
        const postData = route.request().postDataJSON();
        expect(postData.text).toBe('[emotion:pride] Hello world');
        expect(postData.temperature).toBe(0.95);
        expect(postData.cfg_coef).toBe(1.5);
        await fulfillSilentWav(route);
      });

      await page.locator('button:has-text("Synthesize")').click();
    });
  });

  test.describe('Edge Cases', () => {
    test('[kyutai-only] empty text does not trigger synthesis', async ({ page }) => {
      await requireKyutai(page);
      await page.locator('#tts-input').fill('');
      await page.locator('#tts-emotion').selectOption('amusement');
      await expect(page.locator('button:has-text("Synthesize")')).toBeDisabled();
    });

    test('[kyutai-only] whitespace-only text does not trigger synthesis', async ({ page }) => {
      await requireKyutai(page);
      await page.locator('#tts-input').fill('   ');
      await expect(page.locator('button:has-text("Synthesize")')).toBeDisabled();
    });

    test('[kyutai-only] controls reset when switching to pocket and back to kyutai', async ({ page }) => {
      await requireKyutai(page);
      await page.locator('#tts-emotion').selectOption('desire');
      await page.locator('#tts-temperature').evaluate((el: HTMLInputElement) => {
        el.value = '1.5';
        el.dispatchEvent(new Event('input', { bubbles: true }));
      });

      await page.locator('#tts-engine').selectOption('pocket');
      await page.locator('#tts-engine').selectOption('kyutai');

      await expect(page.locator('#tts-emotion')).toHaveValue('desire');
      await expect(page.locator('#tts-temperature')).toHaveValue('1.5');
    });

    test('[kyutai-only] synthesis with special characters in text and emotion', async ({ page }) => {
      await requireKyutai(page);
      await page.locator('#tts-input').fill('Hello! How are you? [testing] brackets');
      await page.locator('#tts-emotion').selectOption('whisper');

      await page.route('/api/tts/synthesize', async (route) => {
        const postData = route.request().postDataJSON();
        expect(postData.text).toBe('[emotion:whisper] Hello! How are you? [testing] brackets');
        await fulfillSilentWav(route);
      });

      await page.locator('button:has-text("Synthesize")').click();
    });
  });
});
