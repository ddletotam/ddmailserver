const { chromium } = require('playwright');

(async () => {
  const browser = await chromium.launch({ headless: false });
  const context = await browser.newContext();
  const page = await context.newPage();

  try {
    // Login with the test user from previous test
    await page.goto('https://ddm.logdoc.ru/login');
    await page.fill('#login-username', 'testuser_1772288309706');
    await page.fill('#login-password', 'TestPass123!');
    await page.locator('#login-form button[type="submit"]').click();
    await page.waitForURL('**/dashboard', { timeout: 10000 });

    console.log('✓ Logged in');

    // Go to inbox
    await page.goto('https://ddm.logdoc.ru/inbox');
    await page.waitForLoadState('networkidle');
    await page.waitForTimeout(2000);

    const messageCount = await page.locator('.message-item').count();
    console.log(`\n📧 Messages in inbox: ${messageCount}`);

    if (messageCount > 0) {
      // Get first few message subjects
      for (let i = 0; i < Math.min(5, messageCount); i++) {
        const subject = await page.locator('.message-item').nth(i).locator('.message-subject').textContent();
        const from = await page.locator('.message-item').nth(i).locator('.message-from').textContent();
        console.log(`  ${i + 1}. From: ${from.trim()}`);
        console.log(`     Subject: ${subject.trim()}`);
      }
    }

    await page.screenshot({ path: 'test-results/inbox-with-messages.png', fullPage: true });
    console.log('\n✅ Screenshot saved');

    await page.waitForTimeout(10000);

  } catch (error) {
    console.error('Error:', error.message);
  } finally {
    await browser.close();
  }
})();
