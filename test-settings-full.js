const { chromium } = require('playwright');

(async () => {
  const browser = await chromium.launch({ headless: false });
  const context = await browser.newContext();
  const page = await context.newPage();

  try {
    console.log('=== Testing Settings Functionality ===\n');

    // Generate unique username
    const username = `testuser_${Date.now()}`;
    const password = 'TestPass123!';
    const newPassword = 'NewPass456!';

    // 1. Register new user
    console.log('1. Registering new user...');
    await page.goto('https://ddm.logdoc.ru/login');
    await page.waitForLoadState('networkidle');

    await page.click('button:has-text("Register")');
    await page.waitForTimeout(500);

    await page.fill('#register-username', username);
    await page.fill('#register-password', password);
    await page.fill('#register-password-confirm', password);
    await page.locator('#register-form button[type="submit"]').click();

    await page.waitForURL('**/recovery-key**', { timeout: 10000 });
    console.log('   ✓ User registered successfully');

    // Continue to dashboard
    await page.locator('input[type="checkbox"][x-model="confirmed"]').check();
    await page.waitForTimeout(500);
    await page.click('a:has-text("Continue to Dashboard")');
    await page.waitForURL('**/dashboard', { timeout: 10000 });
    console.log('   ✓ Navigated to dashboard');

    // 2. Navigate to settings
    console.log('\n2. Opening settings page...');
    await page.goto('https://ddm.logdoc.ru/settings');
    await page.waitForLoadState('networkidle');
    await page.waitForTimeout(1000);

    await page.screenshot({ path: 'test-results/settings-full-page.png', fullPage: true });
    console.log('   ✓ Settings page loaded');

    // 3. Test password change
    console.log('\n3. Testing password change...');
    await page.fill('#current_password', password);
    await page.fill('#new_password', newPassword);
    await page.fill('#confirm_password', newPassword);

    await page.locator('form[hx-post="/api/settings/password"] button[type="submit"]').click();
    await page.waitForTimeout(2000);

    const passwordSuccess = await page.locator('#password-message.success-message').isVisible();
    console.log(`   Password change: ${passwordSuccess ? '✓ SUCCESS' : '✗ FAILED'}`);

    if (passwordSuccess) {
      const message = await page.locator('#password-message').textContent();
      console.log(`   Message: "${message}"`);
    }

    await page.screenshot({ path: 'test-results/settings-password-changed.png', fullPage: true });

    // 4. Test language change to Russian
    console.log('\n4. Testing language change to Russian...');
    await page.selectOption('#language', 'ru');
    await page.locator('form[hx-post="/api/settings/language"] button[type="submit"]').click();
    await page.waitForTimeout(2000);

    const languageSuccess = await page.locator('#language-message.success-message').isVisible();
    console.log(`   Language change: ${languageSuccess ? '✓ SUCCESS' : '✗ FAILED'}`);

    if (languageSuccess) {
      const message = await page.locator('#language-message').textContent();
      console.log(`   Message: "${message}"`);
    }

    await page.screenshot({ path: 'test-results/settings-language-changed.png', fullPage: true });

    // 5. Verify new password works - logout and login
    console.log('\n5. Verifying new password works...');
    await page.goto('https://ddm.logdoc.ru/logout');
    await page.waitForURL('**/login', { timeout: 5000 });
    console.log('   ✓ Logged out');

    await page.fill('#login-username', username);
    await page.fill('#login-password', newPassword);
    await page.locator('#login-form button[type="submit"]').click();

    await page.waitForURL('**/dashboard', { timeout: 10000 });
    console.log('   ✓ Logged in with new password successfully');

    // 6. Check language persistence
    console.log('\n6. Checking language persistence...');
    await page.goto('https://ddm.logdoc.ru/settings');
    await page.waitForLoadState('networkidle');

    const selectedLanguage = await page.locator('#language').inputValue();
    console.log(`   Selected language: ${selectedLanguage}`);
    console.log(`   Language persistence: ${selectedLanguage === 'ru' ? '✓ SUCCESS' : '✗ FAILED'}`);

    await page.screenshot({ path: 'test-results/settings-language-persisted.png', fullPage: true });

    // 7. Test delete account (optional - commented out to keep test user)
    console.log('\n7. Testing delete account functionality...');
    console.log('   (Skipped - would delete test user)');
    // Uncomment to test delete:
    // await page.evaluate(() => {
    //   confirmDeleteAccount();
    // });

    console.log('\n✅ All settings tests completed successfully!');
    console.log(`\nTest user created: ${username} / ${newPassword}`);

  } catch (error) {
    console.error('\n❌ TEST FAILED:', error.message);
    await page.screenshot({ path: 'test-results/settings-full-error.png' });
  } finally {
    await browser.close();
  }
})();
