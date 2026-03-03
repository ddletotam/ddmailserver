const { chromium } = require('playwright');

(async () => {
  const browser = await chromium.launch({ headless: false });
  const context = await browser.newContext();
  const page = await context.newPage();

  try {
    console.log('=== Testing Registration Without Email ===\n');

    await page.goto('https://mail.letotam.ru/login');

    // Switch to register tab
    await page.click('button:has-text("Register")');
    await page.waitForTimeout(500);

    // Check that email field is NOT present
    const emailField = await page.locator('#register-email').count();
    console.log(`Email field present: ${emailField > 0 ? '❌ YES (should be removed!)' : '✅ NO (correct!)'}`);

    // Fill registration form
    const username = `testuser_${Date.now()}`;
    const password = 'TestPass123!';

    await page.fill('#register-username', username);
    await page.fill('#register-password', password);
    await page.fill('#register-password-confirm', password);

    console.log(`\nRegistering user: ${username}`);

    await page.screenshot({ path: 'test-results/registration-no-email.png', fullPage: true });

    // Submit
    await page.locator('#register-form button[type="submit"]').click();
    await page.waitForURL('**/recovery-key**', { timeout: 10000 });

    console.log('✅ Registration successful!');
    console.log('   Redirected to recovery key page');

    await page.waitForTimeout(3000);

  } catch (error) {
    console.error('\n❌ Error:', error.message);
    await page.screenshot({ path: 'test-results/registration-error.png' });
  } finally {
    await browser.close();
  }
})();
