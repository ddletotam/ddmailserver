const { chromium } = require('playwright');

(async () => {
  const browser = await chromium.launch({ headless: false });
  const context = await browser.newContext();
  const page = await context.newPage();

  try {
    console.log('=== Testing Email Sending ===\n');

    // Login with existing test user
    await page.goto('https://mail.letotam.ru/login');
    await page.fill('#login-username', 'testuser_1772288309706');
    await page.fill('#login-password', 'TestPass123!');
    await page.locator('#login-form button[type="submit"]').click();
    await page.waitForURL('**/dashboard', { timeout: 10000 });
    console.log('✓ Logged in');

    // Go to compose page
    await page.goto('https://mail.letotam.ru/compose');
    await page.waitForLoadState('networkidle');
    console.log('✓ Compose page loaded');

    // Fill in the form
    await page.selectOption('#account_id', { index: 1 }); // Select first account
    console.log('✓ Account selected');

    await page.fill('#to', 'lucky@i2lab.ru'); // Send to ourselves
    await page.fill('#subject', 'Test email from DDMailServer');
    await page.fill('#body', 'This is a test email sent via DDMailServer compose feature.\n\nTesting SMTP functionality!');
    console.log('✓ Form filled');

    await page.screenshot({ path: 'test-results/compose-before-send.png', fullPage: true });

    // Submit form
    console.log('\nSending email...');
    await page.click('button[type="submit"]:has-text("Send Email")');
    await page.waitForTimeout(2000);

    // Check for success message
    const successMessage = await page.locator('#send-status').textContent();
    console.log(`Response: ${successMessage}`);

    await page.screenshot({ path: 'test-results/compose-after-send.png', fullPage: true });

    if (successMessage.includes('queued') || successMessage.includes('✅')) {
      console.log('\n✅ Email queued successfully!');
    } else {
      console.log('\n⚠️  Unexpected response');
    }

    // Wait a bit for email to be sent
    console.log('\nWaiting 10 seconds for email to be sent...');
    await page.waitForTimeout(10000);

    // Go to inbox to check if we received it
    await page.goto('https://mail.letotam.ru/inbox');
    await page.waitForLoadState('networkidle');
    await page.waitForTimeout(3000);

    const messageCount = await page.locator('.message-item').count();
    console.log(`\nMessages in inbox: ${messageCount}`);

    // Check if our test email is there
    const testMessageVisible = await page.locator('.message-subject:has-text("Test email from DDMailServer")').isVisible().catch(() => false);
    if (testMessageVisible) {
      console.log('✅ Test email found in inbox!');
    } else {
      console.log('⚠️  Test email not yet visible (may take time to sync)');
    }

    await page.screenshot({ path: 'test-results/inbox-after-send.png', fullPage: true });

    console.log('\n✅ Test completed!');
    await page.waitForTimeout(5000);

  } catch (error) {
    console.error('\n❌ Error:', error.message);
    await page.screenshot({ path: 'test-results/send-email-error.png' });
  } finally {
    await browser.close();
  }
})();
