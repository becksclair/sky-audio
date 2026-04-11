import { defineConfig, devices } from 'playwright/test';

const liveWebRTCMode = process.env.PLAYWRIGHT_LIVE_WEBRTC === '1';
const fakeAudioCaptureFile = process.env.PLAYWRIGHT_FAKE_AUDIO_FILE || '/home/bex/projects/sky/sky-audio/sky_voice_test.wav';
const liveGatewayBaseURL = process.env.PLAYWRIGHT_LIVE_GATEWAY_BASE_URL || 'http://127.0.0.1:18688/dashboard/';
const liveGatewayHealthURL = process.env.PLAYWRIGHT_LIVE_GATEWAY_HEALTH_URL || 'http://127.0.0.1:18688/health';
const liveGatewaySTTDevice = process.env.PLAYWRIGHT_LIVE_STT_DEVICE || 'cpu';
const liveGatewaySTTSocket = process.env.PLAYWRIGHT_LIVE_STT_SOCKET || '/tmp/sky-audio-playwright-stt.sock';
const liveGatewayCommand =
  process.env.PLAYWRIGHT_LIVE_GATEWAY_COMMAND ||
  `cd .. && exec env SKY_AUDIO_API_KEY=playwright-live PORT=18688 STT_DEVICE=${liveGatewaySTTDevice} SKY_AUDIO_STT_SOCKET=${liveGatewaySTTSocket} ./gateway/sky-audio`;
const defaultBaseURL = liveWebRTCMode ? liveGatewayBaseURL : 'http://localhost:5173';

export default defineConfig({
  testDir: './e2e',
  fullyParallel: true,
  forbidOnly: !!process.env.CI,
  retries: process.env.CI ? 2 : 0,
  workers: process.env.CI ? 1 : undefined,
  reporter: [
    ['list'],
    ['html', { outputFolder: 'e2e-report', open: 'never' }],
  ],
  use: {
    baseURL: process.env.PLAYWRIGHT_BASE_URL || defaultBaseURL,
    trace: 'on-first-retry',
    screenshot: 'only-on-failure',
    video: 'retain-on-failure',
  },
  projects: [
    {
      name: 'chromium',
      use: {
        ...devices['Desktop Chrome'],
        permissions: ['microphone'],
        ...(liveWebRTCMode
          ? {
              launchOptions: {
                args: [
                  '--use-fake-ui-for-media-stream',
                  '--use-fake-device-for-media-stream',
                  `--use-file-for-fake-audio-capture=${fakeAudioCaptureFile}`,
                ],
              },
            }
          : {}),
      },
    },
  ],
  webServer: liveWebRTCMode
    ? {
        command: liveGatewayCommand,
        url: liveGatewayHealthURL,
        reuseExistingServer: false,
        timeout: 600000,
        gracefulShutdown: {
          signal: 'SIGTERM',
          timeout: 10000,
        },
      }
    : {
        command: 'npm run dev',
        url: 'http://localhost:5173',
        reuseExistingServer: !process.env.CI,
        timeout: 120000,
      },
});
