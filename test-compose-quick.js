const { chromium } = require('playwright');

(async () => {
  const browser = await chromium.launch({ headless: false });
  const context = await browser.newContext();
  const page = await context.newPage();

  try {
    // Login with existing user
    await page.goto('https://ddm.logdoc.ru/login');
    await page.fill('#login-username', 'testuser_1772287430415');
    await page.fill('#login-password', 'TestPass123!');
    await page.locator('#login-form button[type="submit"]').click();
    await page.waitForURL('**/dashboard', { timeout: 10000 });

    // Go to compose
    await page.goto('https://ddm.logdoc.ru/compose');
    await page.waitForLoadState('networkidle');
    await page.waitForTimeout(1000);

    await page.screenshot({ path: 'test-results/compose-fixed.png', fullPage: true });
    console.log('✓ Compose page screenshot saved');

  } catch (error) {
    console.error('Error:', error.message);
  } finally {
    await browser.close();
  }
})();
