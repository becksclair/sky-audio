# Dashboard E2E Tests

This directory contains Playwright end-to-end tests for the Sky Audio Dashboard.

## Test Coverage

### TTS Emotion and Synthesis Controls

- **`tts-emotion-controls.spec.ts`** - UI behavior tests
  - Emotion combobox visibility (Kyutai vs Pocket)
  - Synthesis slider controls (temperature, cfg_coef, padding)
  - Integration between engine switching and control visibility
  - State persistence across engine switches
  - Help text and default value display

- **`tts-model-behavior.spec.ts`** - Model/request behavior tests
  - Emotion tag prepending to text
  - Synthesis options in request payload
  - Integration between emotion and synthesis options
  - Edge cases (empty text, special characters)

## Running Tests

```bash
# Install Playwright browsers (one-time)
npm run e2e:install

# Run all e2e tests
npm run e2e

# Run tests with UI mode (for debugging)
npm run e2e:ui

# Run tests with debug mode
npm run e2e:debug

# Run specific test file
npx playwright test tts-emotion-controls.spec.ts

# Run tests in headed mode (see browser)
npx playwright test --headed
```

## Test Configuration

Tests are configured in `playwright.config.ts`:
- Base URL: http://localhost:5173 (dev server)
- Browsers: Chromium, Firefox, WebKit
- Parallel execution enabled (except in CI)
- Auto-starts dev server before tests

## Writing New Tests

1. Create a new `.spec.ts` file in `dashboard/e2e/`
2. Import `{ test, expect } from '@playwright/test'`
3. Use `page.route()` to mock API calls
4. Use `test.describe()` for organization

## Key Patterns

### Mocking the TTS API
```typescript
await page.route('/api/tts/synthesize', async (route) => {
  const postData = route.request().postDataJSON();
  expect(postData.text).toBe('[emotion:amusement] Hello');
  expect(postData.temperature).toBe(0.95);
  await route.fulfill({
    status: 200,
    contentType: 'audio/wav',
    body: Buffer.from('fake-audio-data'),
  });
});
```

### Setting Slider Values
```typescript
await page.locator('#tts-temperature').evaluate((el: HTMLInputElement) => {
  el.value = '1.25';
  el.dispatchEvent(new Event('input', { bubbles: true }));
});
```
