import { test, expect, type Page } from '@playwright/test';

let kyutaiCatalogAvailable = false;

async function requireKyutai(page: Page): Promise<void> {
  test.skip(!kyutaiCatalogAvailable, 'Kyutai engine is not available in this environment.');
  await page.locator('#tts-engine').selectOption('kyutai');
}

test.describe('TTS Emotion and Synthesis Controls', () => {
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

  test.describe('Emotion Controls', () => {
    test('emotion combobox is not visible for pocket engine', async ({ page }) => {
      await page.locator('#tts-engine').selectOption('pocket');
      await expect(page.locator('#tts-emotion')).not.toBeVisible();
    });

    test('[kyutai-only] emotion combobox is visible for kyutai engine', async ({ page }) => {
      await requireKyutai(page);
      await expect(page.locator('#tts-emotion')).toBeVisible();
      await expect(page.locator('#tts-emotion')).toHaveValue('not-set');
    });

    test('[kyutai-only] emotion combobox has all 9 emotions plus not-set option', async ({ page }) => {
      await requireKyutai(page);

      const expectedEmotions = [
        'not-set',
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

      for (const emotion of expectedEmotions) {
        await expect(page.locator(`#tts-emotion option[value="${emotion}"]`)).toHaveCount(1);
      }
    });

    test('[kyutai-only] selecting emotion updates the value', async ({ page }) => {
      await requireKyutai(page);

      await page.locator('#tts-emotion').selectOption('amusement');
      await expect(page.locator('#tts-emotion')).toHaveValue('amusement');

      await page.locator('#tts-emotion').selectOption('whisper');
      await expect(page.locator('#tts-emotion')).toHaveValue('whisper');

      await page.locator('#tts-emotion').selectOption('not-set');
      await expect(page.locator('#tts-emotion')).toHaveValue('not-set');
    });

    test('[kyutai-only] emotion label shows capitalized text', async ({ page }) => {
      await requireKyutai(page);

      await expect(page.locator('#tts-emotion option[value="not-set"]')).toHaveText('Not Set');
      await expect(page.locator('#tts-emotion option[value="amusement"]')).toHaveText('Amusement');
    });
  });

  test.describe('Synthesis Sliders', () => {
    test('sliders are not visible for pocket engine', async ({ page }) => {
      await page.locator('#tts-engine').selectOption('pocket');

      await expect(page.locator('#tts-temperature')).not.toBeVisible();
      await expect(page.locator('#tts-cfg-coef')).not.toBeVisible();
      await expect(page.locator('#tts-padding')).not.toBeVisible();
    });

    test('[kyutai-only] sliders are visible for kyutai engine', async ({ page }) => {
      await requireKyutai(page);

      await expect(page.locator('#tts-temperature')).toBeVisible();
      await expect(page.locator('#tts-cfg-coef')).toBeVisible();
      await expect(page.locator('#tts-padding')).toBeVisible();
    });

    test('[kyutai-only] temperature slider has correct defaults and range', async ({ page }) => {
      await requireKyutai(page);
      const slider = page.locator('#tts-temperature');

      await expect(slider).toHaveAttribute('min', '0');
      await expect(slider).toHaveAttribute('max', '2');
      await expect(slider).toHaveAttribute('step', '0.05');
      await expect(slider).toHaveValue('0.75');
      await expect(page.getByText('0.75', { exact: true })).toBeVisible();
    });

    test('[kyutai-only] cfg-coef slider has correct defaults and range', async ({ page }) => {
      await requireKyutai(page);
      const slider = page.locator('#tts-cfg-coef');

      await expect(slider).toHaveAttribute('min', '0');
      await expect(slider).toHaveAttribute('max', '3');
      await expect(slider).toHaveAttribute('step', '0.1');
      await expect(slider).toHaveValue('1.5');
      await expect(page.locator('text=1.50')).toBeVisible();
    });

    test('[kyutai-only] padding slider has correct defaults and range', async ({ page }) => {
      await requireKyutai(page);
      const slider = page.locator('#tts-padding');

      await expect(slider).toHaveAttribute('min', '0');
      await expect(slider).toHaveAttribute('max', '10');
      await expect(slider).toHaveAttribute('step', '1');
      await expect(slider).toHaveValue('0');
      await expect(page.locator('#tts-padding').locator('..').locator('span').filter({ hasText: '0' })).toHaveCount(1);
    });

    test('[kyutai-only] changing temperature slider updates displayed value', async ({ page }) => {
      await requireKyutai(page);
      const slider = page.locator('#tts-temperature');

      await slider.evaluate((el: HTMLInputElement) => {
        el.value = '1.25';
        el.dispatchEvent(new Event('input', { bubbles: true }));
      });

      await expect(slider).toHaveValue('1.25');
    });

    test('[kyutai-only] changing cfg-coef slider updates displayed value', async ({ page }) => {
      await requireKyutai(page);
      const slider = page.locator('#tts-cfg-coef');

      await slider.evaluate((el: HTMLInputElement) => {
        el.value = '2.0';
        el.dispatchEvent(new Event('input', { bubbles: true }));
      });

      await expect(slider).toHaveValue('2');
    });

    test('[kyutai-only] changing padding slider updates displayed value', async ({ page }) => {
      await requireKyutai(page);
      const slider = page.locator('#tts-padding');

      await slider.evaluate((el: HTMLInputElement) => {
        el.value = '5';
        el.dispatchEvent(new Event('input', { bubbles: true }));
      });

      await expect(slider).toHaveValue('5');
    });
  });

  test.describe('Integration', () => {
    test('[kyutai-only] switching engine shows/hides kyutai controls', async ({ page }) => {
      test.skip(!kyutaiCatalogAvailable, 'Kyutai engine is not available in this environment.');

      await page.locator('#tts-engine').selectOption('pocket');
      await expect(page.locator('#tts-emotion')).not.toBeVisible();

      await page.locator('#tts-engine').selectOption('kyutai');
      await expect(page.locator('#tts-emotion')).toBeVisible();
      await expect(page.locator('#tts-temperature')).toBeVisible();

      await page.locator('#tts-engine').selectOption('pocket');
      await expect(page.locator('#tts-emotion')).not.toBeVisible();
      await expect(page.locator('#tts-temperature')).not.toBeVisible();
    });

    test('[kyutai-only] synthesize button state with emotion selected', async ({ page }) => {
      await requireKyutai(page);
      await page.locator('#tts-input').fill('Hello world');
      await page.locator('#tts-emotion').selectOption('amusement');
      await expect(page.locator('button:has-text("Synthesize")')).toBeEnabled();
    });

    test('[kyutai-only] help text explains emotion tag behavior', async ({ page }) => {
      await requireKyutai(page);
      await expect(page.locator('text=Adds [emotion:label] tag')).toBeVisible();
    });

    test('[kyutai-only] default labels are shown for synthesis parameters', async ({ page }) => {
      await requireKyutai(page);
      await expect(page.getByText('Default: 0.75', { exact: true })).toBeVisible();
      await expect(page.getByText('Default: 1.5', { exact: true })).toBeVisible();
      await expect(page.getByText('Default: 0', { exact: true })).toBeVisible();
    });
  });

  test.describe('State Persistence', () => {
    test('[kyutai-only] emotion selection persists when switching engines', async ({ page }) => {
      await requireKyutai(page);
      await page.locator('#tts-emotion').selectOption('desire');

      await page.locator('#tts-engine').selectOption('pocket');
      await page.locator('#tts-engine').selectOption('kyutai');

      await expect(page.locator('#tts-emotion')).toHaveValue('desire');
    });

    test('[kyutai-only] slider values persist when switching engines', async ({ page }) => {
      await requireKyutai(page);

      await page.locator('#tts-temperature').evaluate((el: HTMLInputElement) => {
        el.value = '1.5';
        el.dispatchEvent(new Event('input', { bubbles: true }));
      });

      await page.locator('#tts-engine').selectOption('pocket');
      await page.locator('#tts-engine').selectOption('kyutai');

      await expect(page.locator('#tts-temperature')).toHaveValue('1.5');
    });
  });
});
