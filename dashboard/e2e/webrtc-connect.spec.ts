import { test, expect } from '@playwright/test';

test.describe('WebRTC Connect Flow', () => {
  test('connects successfully and updates UI status', async ({ page }) => {
    await page.addInitScript(() => {
      const fakeTrack = {
        id: 'fake-mic-track',
        kind: 'audio',
        stop() {
          // no-op
        },
      };

      const fakeMediaStream = {
        getTracks() {
          return [fakeTrack];
        },
      };

      if (!navigator.mediaDevices) {
        Object.defineProperty(navigator, 'mediaDevices', {
          configurable: true,
          value: {},
        });
      }

      navigator.mediaDevices.getUserMedia = async () => fakeMediaStream as unknown as MediaStream;

      class FakeDataChannel {
        label: string;
        readyState: RTCDataChannelState = 'open';
        onmessage: ((event: MessageEvent) => void) | null = null;
        onclose: (() => void) | null = null;

        constructor(label: string) {
          this.label = label;
        }

        send() {
          // no-op
        }

        close() {
          this.readyState = 'closed';
          this.onclose?.();
        }
      }

      class FakeRTCPeerConnection {
        localDescription: RTCSessionDescriptionInit | null = null;
        remoteDescription: RTCSessionDescriptionInit | null = null;
        iceGatheringState: RTCIceGatheringState = 'complete';
        iceConnectionState: RTCIceConnectionState = 'new';
        ondatachannel: ((event: RTCDataChannelEvent) => void) | null = null;
        ontrack: ((event: RTCTrackEvent) => void) | null = null;
        oniceconnectionstatechange: (() => void) | null = null;

        addTrack() {
          // no-op
        }

        createDataChannel(label: string): RTCDataChannel {
          return new FakeDataChannel(label) as unknown as RTCDataChannel;
        }

        async createOffer(): Promise<RTCSessionDescriptionInit> {
          return { type: 'offer', sdp: 'v=0\r\ns=fake-offer\r\n' };
        }

        async setLocalDescription(desc: RTCSessionDescriptionInit): Promise<void> {
          this.localDescription = desc;
        }

        addEventListener() {
          // no-op
        }

        removeEventListener() {
          // no-op
        }

        async setRemoteDescription(desc: RTCSessionDescriptionInit): Promise<void> {
          this.remoteDescription = desc;
        }

        close() {
          this.iceConnectionState = 'closed';
          this.oniceconnectionstatechange?.();
        }
      }

      Object.defineProperty(window, 'RTCPeerConnection', {
        configurable: true,
        writable: true,
        value: FakeRTCPeerConnection,
      });
    });

    await page.route('**/v1/realtime/calls', async (route) => {
      await route.fulfill({
        status: 200,
        headers: {
          Location: '/v1/realtime/sessions/test-session-e2e',
        },
        contentType: 'application/sdp',
        body: 'v=0\r\ns=fake-answer\r\n',
      });
    });

    await page.goto('/');
    await expect(page.locator('h1')).toContainText('Sky Audio Playground');

    await page.locator('button:has-text("Connect WebRTC")').click();

    const statusLine = page.locator('p:has-text("Status:")');
    await expect(page.locator('button:has-text("Disconnect")')).toBeVisible();
    await expect(statusLine).toContainText('connected');
    await expect(statusLine).toContainText('test-session-e2e');
    await expect(page.locator('.htk-chat')).toContainText('Connected. Speak into your microphone.');

    await page.locator('button:has-text("Disconnect")').click();
    await expect(page.locator('button:has-text("Connect WebRTC")')).toBeVisible();
    await expect(statusLine).toContainText('disconnected');
  });
});
