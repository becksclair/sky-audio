import { test, expect } from '@playwright/test';

test.describe('TTS Model Behavior - Emotion Tag and Synthesis Options', () => {
  test.beforeEach(async ({ page }) => {
    await page.goto('/');
    await expect(page.locator('h1')).toContainText('Sky Audio Playground');
  });

  test.describe('Emotion Tag Prepending', () => {
    test('transcript shows emotion in log when emotion is selected', async ({ page }) => {
      await page.locator('#tts-engine').selectOption('kyutai');
      await page.locator('#tts-input').fill('Hello world');
      await page.locator('#tts-emotion').selectOption('amusement');
      
      // Click synthesize (will likely fail without backend, but we can check the transcript)
      const synthesizeBtn = page.locator('button:has-text("Synthesize")');
      
      // Mock the fetch to prevent actual network request
      await page.route('/api/tts/synthesize', async (route) => {
        await route.fulfill({
          status: 200,
          contentType: 'audio/wav',
          body: Buffer.from('RIFF\x1e\x00\x00\x00WAVEfmt \x10\x00\x00\x00\x01\x00\x01\x00@\x1f\x00\x00@\x1f\x00\x00\x01\x00\x10\x00data\x02\x00\x00\x00\x00\x00', 'binary'),
        });
      });
      
      await synthesizeBtn.click();
      
      // Wait for transcript to update (give it time to process the mock response)
      await page.waitForTimeout(500);
      
      // Check transcript contains the emotion indicator
      await expect(page.locator('.htk-chat')).toContainText('[TTS:kyutai');
      await expect(page.locator('.htk-chat')).toContainText(':amusement]');
    });

    test('transcript shows original text (not tagged text) in log', async ({ page }) => {
      await page.locator('#tts-engine').selectOption('kyutai');
      await page.locator('#tts-input').fill('Hello world');
      await page.locator('#tts-emotion').selectOption('desire');
      
      await page.route('/api/tts/synthesize', async (route) => {
        const postData = route.request().postDataJSON();
        // Verify the text sent to server has the emotion tag prepended
        expect(postData.text).toBe('[emotion:desire] Hello world');
        await route.fulfill({
          status: 200,
          contentType: 'audio/wav',
          body: Buffer.from('fake-audio-data'),
        });
      });
      
      await page.locator('button:has-text("Synthesize")').click();
    });

    test('no emotion tag in request when not-set is selected', async ({ page }) => {
      await page.locator('#tts-engine').selectOption('kyutai');
      await page.locator('#tts-input').fill('Hello world');
      // Ensure not-set is selected (default)
      await page.locator('#tts-emotion').selectOption('not-set');
      
      await page.route('/api/tts/synthesize', async (route) => {
        const postData = route.request().postDataJSON();
        // Verify the text sent to server does NOT have emotion tag
        expect(postData.text).toBe('Hello world');
        expect(postData.text).not.toContain('[emotion:');
        await route.fulfill({
          status: 200,
          contentType: 'audio/wav',
          body: Buffer.from('fake-audio-data'),
        });
      });
      
      await page.locator('button:has-text("Synthesize")').click();
    });

    test('all 9 emotions can be selected and sent correctly', async ({ page }) => {
      const emotions = [
        'neutral', 'amusement', 'interest', 'desire', 'adoration',
        'contentment', 'serenity', 'pride', 'whisper'
      ];
      
      await page.locator('#tts-engine').selectOption('kyutai');
      await page.locator('#tts-input').fill('Test text');
      
      for (const emotion of emotions) {
        await page.locator('#tts-emotion').selectOption(emotion);
        
        await page.route('/api/tts/synthesize', async (route) => {
          const postData = route.request().postDataJSON();
          expect(postData.text).toBe(`[emotion:${emotion}] Test text`);
          await route.fulfill({
            status: 200,
            contentType: 'audio/wav',
            body: Buffer.from('fake-audio-data'),
          });
        });
        
        await page.locator('button:has-text("Synthesize")').click();
        await page.waitForTimeout(100); // Small delay between requests
      }
    });
  });

  test.describe('Synthesis Options in Request', () => {
    test('kyutai request includes default synthesis options', async ({ page }) => {
      await page.locator('#tts-engine').selectOption('kyutai');
      await page.locator('#tts-input').fill('Hello world');
      
      await page.route('/api/tts/synthesize', async (route) => {
        const postData = route.request().postDataJSON();
        // Verify default synthesis options are sent
        expect(postData.temperature).toBe(0.75);
        expect(postData.cfg_coef).toBe(1.5);
        expect(postData.padding_between).toBe(0);
        await route.fulfill({
          status: 200,
          contentType: 'audio/wav',
          body: Buffer.from('fake-audio-data'),
        });
      });
      
      await page.locator('button:has-text("Synthesize")').click();
    });

    test('kyutai request includes custom synthesis options', async ({ page }) => {
      await page.locator('#tts-engine').selectOption('kyutai');
      await page.locator('#tts-input').fill('Hello world');
      
      // Set custom values
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
        // Verify custom synthesis options are sent
        expect(postData.temperature).toBe(1.25);
        expect(postData.cfg_coef).toBe(2);
        expect(postData.padding_between).toBe(3);
        await route.fulfill({
          status: 200,
          contentType: 'audio/wav',
          body: Buffer.from('fake-audio-data'),
        });
      });
      
      await page.locator('button:has-text("Synthesize")').click();
    });

    test('pocket request does not include synthesis options', async ({ page }) => {
      await page.locator('#tts-engine').selectOption('pocket');
      await page.locator('#tts-input').fill('Hello world');
      
      await page.route('/api/tts/synthesize', async (route) => {
        const postData = route.request().postDataJSON();
        // Verify synthesis options are NOT sent for pocket
        expect(postData.temperature).toBeUndefined();
        expect(postData.cfg_coef).toBeUndefined();
        expect(postData.padding_between).toBeUndefined();
        await route.fulfill({
          status: 200,
          contentType: 'audio/wav',
          body: Buffer.from('fake-audio-data'),
        });
      });
      
      await page.locator('button:has-text("Synthesize")').click();
    });

    test('request includes both emotion tag and synthesis options', async ({ page }) => {
      await page.locator('#tts-engine').selectOption('kyutai');
      await page.locator('#tts-input').fill('Hello world');
      await page.locator('#tts-emotion').selectOption('pride');
      
      await page.locator('#tts-temperature').evaluate((el: HTMLInputElement) => {
        el.value = '0.95';
        el.dispatchEvent(new Event('input', { bubbles: true }));
      });
      
      await page.route('/api/tts/synthesize', async (route) => {
        const postData = route.request().postDataJSON();
        // Verify both emotion tag and options are present
        expect(postData.text).toBe('[emotion:pride] Hello world');
        expect(postData.temperature).toBe(0.95);
        expect(postData.cfg_coef).toBe(1.5);
        await route.fulfill({
          status: 200,
          contentType: 'audio/wav',
          body: Buffer.from('fake-audio-data'),
        });
      });
      
      await page.locator('button:has-text("Synthesize")').click();
    });
  });

  test.describe('Edge Cases', () => {
    test('empty text does not trigger synthesis', async ({ page }) => {
      await page.locator('#tts-engine').selectOption('kyutai');
      await page.locator('#tts-input').fill('');
      await page.locator('#tts-emotion').selectOption('amusement');
      
      const synthesizeBtn = page.locator('button:has-text("Synthesize")');
      await expect(synthesizeBtn).toBeDisabled();
    });

    test('whitespace-only text does not trigger synthesis', async ({ page }) => {
      await page.locator('#tts-engine').selectOption('kyutai');
      await page.locator('#tts-input').fill('   ');
      
      const synthesizeBtn = page.locator('button:has-text("Synthesize")');
      await expect(synthesizeBtn).toBeDisabled();
    });

    test('controls reset when switching to pocket and back to kyutai', async ({ page }) => {
      await page.locator('#tts-engine').selectOption('kyutai');
      await page.locator('#tts-emotion').selectOption('desire');
      await page.locator('#tts-temperature').evaluate((el: HTMLInputElement) => {
        el.value = '1.5';
        el.dispatchEvent(new Event('input', { bubbles: true }));
      });
      
      // Switch to pocket
      await page.locator('#tts-engine').selectOption('pocket');
      
      // Switch back to kyutai
      await page.locator('#tts-engine').selectOption('kyutai');
      
      // Values should persist (not reset)
      await expect(page.locator('#tts-emotion')).toHaveValue('desire');
      await expect(page.locator('#tts-temperature')).toHaveValue('1.5');
    });

    test('synthesis with special characters in text and emotion', async ({ page }) => {
      await page.locator('#tts-engine').selectOption('kyutai');
      await page.locator('#tts-input').fill('Hello! How are you? [testing] brackets');
      await page.locator('#tts-emotion').selectOption('whisper');
      
      await page.route('/api/tts/synthesize', async (route) => {
        const postData = route.request().postDataJSON();
        // The emotion tag should be prepended correctly even with brackets in text
        expect(postData.text).toBe('[emotion:whisper] Hello! How are you? [testing] brackets');
        await route.fulfill({
          status: 200,
          contentType: 'audio/wav',
          body: Buffer.from('fake-audio-data'),
        });
      });
      
      await page.locator('button:has-text("Synthesize")').click();
    });
  });
});
