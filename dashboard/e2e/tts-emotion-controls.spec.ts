import { test, expect } from '@playwright/test';

test.describe('TTS Emotion and Synthesis Controls', () => {
  test.beforeEach(async ({ page }) => {
    await page.goto('/');
    // Wait for the app to load
    await expect(page.locator('h1')).toContainText('Sky Audio Playground');
  });

  test.describe('Emotion Controls', () => {
    test('emotion combobox is not visible for pocket engine', async ({ page }) => {
      // Select pocket engine
      await page.locator('#tts-engine').selectOption('pocket');
      
      // Emotion controls should not be visible
      await expect(page.locator('#tts-emotion')).not.toBeVisible();
    });

    test('emotion combobox is visible for kyutai engine', async ({ page }) => {
      // Select kyutai engine
      await page.locator('#tts-engine').selectOption('kyutai');
      
      // Emotion controls should be visible
      await expect(page.locator('#tts-emotion')).toBeVisible();
      
      // Verify default value is "not-set"
      await expect(page.locator('#tts-emotion')).toHaveValue('not-set');
    });

    test('emotion combobox has all 9 emotions plus not-set option', async ({ page }) => {
      await page.locator('#tts-engine').selectOption('kyutai');
      
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
      
      // Check that all expected emotions exist as options
      for (const emotion of expectedEmotions) {
        const option = page.locator(`#tts-emotion option[value="${emotion}"]`);
        await expect(option).toHaveCount(1);
      }
    });

    test('selecting emotion updates the value', async ({ page }) => {
      await page.locator('#tts-engine').selectOption('kyutai');
      
      // Select amusement emotion
      await page.locator('#tts-emotion').selectOption('amusement');
      await expect(page.locator('#tts-emotion')).toHaveValue('amusement');
      
      // Select whisper emotion
      await page.locator('#tts-emotion').selectOption('whisper');
      await expect(page.locator('#tts-emotion')).toHaveValue('whisper');
      
      // Reset to not-set
      await page.locator('#tts-emotion').selectOption('not-set');
      await expect(page.locator('#tts-emotion')).toHaveValue('not-set');
    });

    test('emotion label shows capitalized text', async ({ page }) => {
      await page.locator('#tts-engine').selectOption('kyutai');
      
      // Check that "Not Set" is displayed for not-set value
      const notSetOption = page.locator('#tts-emotion option[value="not-set"]');
      await expect(notSetOption).toHaveText('Not Set');
      
      // Check that emotions are capitalized
      const amusementOption = page.locator('#tts-emotion option[value="amusement"]');
      await expect(amusementOption).toHaveText('Amusement');
    });
  });

  test.describe('Synthesis Sliders', () => {
    test('sliders are not visible for pocket engine', async ({ page }) => {
      await page.locator('#tts-engine').selectOption('pocket');
      
      // All synthesis sliders should be hidden
      await expect(page.locator('#tts-temperature')).not.toBeVisible();
      await expect(page.locator('#tts-cfg-coef')).not.toBeVisible();
      await expect(page.locator('#tts-padding')).not.toBeVisible();
    });

    test('sliders are visible for kyutai engine', async ({ page }) => {
      await page.locator('#tts-engine').selectOption('kyutai');
      
      // All synthesis sliders should be visible
      await expect(page.locator('#tts-temperature')).toBeVisible();
      await expect(page.locator('#tts-cfg-coef')).toBeVisible();
      await expect(page.locator('#tts-padding')).toBeVisible();
    });

    test('temperature slider has correct defaults and range', async ({ page }) => {
      await page.locator('#tts-engine').selectOption('kyutai');
      
      const slider = page.locator('#tts-temperature');
      
      // Check attributes
      await expect(slider).toHaveAttribute('min', '0');
      await expect(slider).toHaveAttribute('max', '2');
      await expect(slider).toHaveAttribute('step', '0.05');
      
      // Check default value
      await expect(slider).toHaveValue('0.75');
      
      // Check displayed value (the value label, not the default label)
      await expect(page.getByText('0.75', { exact: true })).toBeVisible();
    });

    test('cfg-coef slider has correct defaults and range', async ({ page }) => {
      await page.locator('#tts-engine').selectOption('kyutai');
      
      const slider = page.locator('#tts-cfg-coef');
      
      // Check attributes
      await expect(slider).toHaveAttribute('min', '0');
      await expect(slider).toHaveAttribute('max', '3');
      await expect(slider).toHaveAttribute('step', '0.1');
      
      // Check default value
      await expect(slider).toHaveValue('1.5');
      
      // Check displayed value
      await expect(page.locator('text=1.50')).toBeVisible();
    });

    test('padding slider has correct defaults and range', async ({ page }) => {
      await page.locator('#tts-engine').selectOption('kyutai');
      
      const slider = page.locator('#tts-padding');
      
      // Check attributes
      await expect(slider).toHaveAttribute('min', '0');
      await expect(slider).toHaveAttribute('max', '10');
      await expect(slider).toHaveAttribute('step', '1');
      
      // Check default value
      await expect(slider).toHaveValue('0');
      
      // Check displayed value (the value label span)
      const valueLabel = page.locator('#tts-padding').locator('..').locator('span').filter({ hasText: '0' });
      await expect(valueLabel).toHaveCount(1);
    });

    test('changing temperature slider updates displayed value', async ({ page }) => {
      await page.locator('#tts-engine').selectOption('kyutai');
      
      const slider = page.locator('#tts-temperature');
      
      // Change value using JavaScript (range inputs can be tricky)
      await slider.evaluate((el: HTMLInputElement) => {
        el.value = '1.25';
        el.dispatchEvent(new Event('input', { bubbles: true }));
      });
      
      // Verify value changed
      await expect(slider).toHaveValue('1.25');
    });

    test('changing cfg-coef slider updates displayed value', async ({ page }) => {
      await page.locator('#tts-engine').selectOption('kyutai');
      
      const slider = page.locator('#tts-cfg-coef');
      
      await slider.evaluate((el: HTMLInputElement) => {
        el.value = '2.0';
        el.dispatchEvent(new Event('input', { bubbles: true }));
      });
      
      await expect(slider).toHaveValue('2');
    });

    test('changing padding slider updates displayed value', async ({ page }) => {
      await page.locator('#tts-engine').selectOption('kyutai');
      
      const slider = page.locator('#tts-padding');
      
      await slider.evaluate((el: HTMLInputElement) => {
        el.value = '5';
        el.dispatchEvent(new Event('input', { bubbles: true }));
      });
      
      await expect(slider).toHaveValue('5');
    });
  });

  test.describe('Integration', () => {
    test('switching engine shows/hides kyutai controls', async ({ page }) => {
      // Start with pocket (controls hidden)
      await page.locator('#tts-engine').selectOption('pocket');
      await expect(page.locator('#tts-emotion')).not.toBeVisible();
      
      // Switch to kyutai (controls visible)
      await page.locator('#tts-engine').selectOption('kyutai');
      await expect(page.locator('#tts-emotion')).toBeVisible();
      await expect(page.locator('#tts-temperature')).toBeVisible();
      
      // Switch back to pocket (controls hidden)
      await page.locator('#tts-engine').selectOption('pocket');
      await expect(page.locator('#tts-emotion')).not.toBeVisible();
      await expect(page.locator('#tts-temperature')).not.toBeVisible();
    });

    test('synthesize button state with emotion selected', async ({ page }) => {
      await page.locator('#tts-engine').selectOption('kyutai');
      
      // Enter text
      await page.locator('#tts-input').fill('Hello world');
      
      // Select emotion
      await page.locator('#tts-emotion').selectOption('amusement');
      
      // Synthesize button should be enabled
      const synthesizeBtn = page.locator('button:has-text("Synthesize")');
      await expect(synthesizeBtn).toBeEnabled();
    });

    test('help text explains emotion tag behavior', async ({ page }) => {
      await page.locator('#tts-engine').selectOption('kyutai');
      
      // Check for help text about emotion tag
      await expect(page.locator('text=Adds [emotion:label] tag')).toBeVisible();
    });

    test('default labels are shown for synthesis parameters', async ({ page }) => {
      await page.locator('#tts-engine').selectOption('kyutai');
      
      // Check for default value labels using exact text matching
      await expect(page.getByText('Default: 0.75', { exact: true })).toBeVisible();
      await expect(page.getByText('Default: 1.5', { exact: true })).toBeVisible();
      await expect(page.getByText('Default: 0', { exact: true })).toBeVisible();
    });
  });

  test.describe('State Persistence', () => {
    test('emotion selection persists when switching engines', async ({ page }) => {
      await page.locator('#tts-engine').selectOption('kyutai');
      await page.locator('#tts-emotion').selectOption('desire');
      
      // Switch to pocket and back
      await page.locator('#tts-engine').selectOption('pocket');
      await page.locator('#tts-engine').selectOption('kyutai');
      
      // Emotion should still be desire
      await expect(page.locator('#tts-emotion')).toHaveValue('desire');
    });

    test('slider values persist when switching engines', async ({ page }) => {
      await page.locator('#tts-engine').selectOption('kyutai');
      
      // Change temperature
      await page.locator('#tts-temperature').evaluate((el: HTMLInputElement) => {
        el.value = '1.5';
        el.dispatchEvent(new Event('input', { bubbles: true }));
      });
      
      // Switch to pocket and back
      await page.locator('#tts-engine').selectOption('pocket');
      await page.locator('#tts-engine').selectOption('kyutai');
      
      // Temperature should persist
      await expect(page.locator('#tts-temperature')).toHaveValue('1.5');
    });
  });
});
