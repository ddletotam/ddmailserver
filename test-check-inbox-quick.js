const { chromium } = require('playwright');

(async () => {
  const browser = await chromium.launch({ headless: false });
  const context = await browser.newContext();
  const page = await context.newPage();

  try {
    await page.goto('https://ddm.logdoc.ru/login');
    await page.fill('#login-username', 'testuser_1772288309706');
    await page.fill('#login-password', 'TestPass123!');
    await page.locator('#login-form button[type="submit"]').click();
    await page.waitForURL('**/dashboard', { timeout: 10000 });

    await page.goto('https://ddm.logdoc.ru/inbox');
    await page.waitForLoadState('networkidle');
    await page.waitForTimeout(2000);

    const messageCount = await page.locator('.message-item').count();
    console.log(`Messages in inbox: ${messageCount}`);

    // Look for our test email
    const testEmail = await page.locator('.message-subject:has-text("Test email from DDMailServer")').count();
    if (testEmail > 0) {
      console.log('✅ Found test email!');
      const from = await page.locator('.message-item:has-text("Test email from DDMailServer") .message-from').textContent();
      console.log(`From: ${from}`);
    } else {
      console.log('❌ Test email not found yet');
      // Show last 5 subjects
      for (let i = 0; i < Math.min(5, messageCount); i++) {
        const subject = await page.locator('.message-item').nth(i).locator('.message-subject').textContent();
        console.log(`  ${i + 1}. ${subject.trim()}`);
      }
    }

    await page.waitForTimeout(10000);

  } catch (error) {
    console.error('Error:', error.message);
  } finally {
    await browser.close();
  }
})();
