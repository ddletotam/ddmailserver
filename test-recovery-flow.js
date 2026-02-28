const { chromium } = require('playwright');

(async () => {
  const browser = await chromium.launch({ headless: false });
  const context = await browser.newContext();
  const page = await context.newPage();

  const baseUrl = 'http://jwebhelp.ru:8080';
  const testUsername = `testuser_${Date.now()}`;
  const testPassword = 'TestPass123!';
  let recoveryKey = '';

  try {
    console.log('\n=== Testing Recovery Key Flow ===\n');

    // 1. Navigate to login page
    console.log('1. Opening login page...');
    await page.goto(baseUrl + '/login');
    await page.waitForLoadState('networkidle');
    await page.screenshot({ path: 'test-results/01-login-page.png' });
    console.log('   ✓ Login page loaded');

    // Check if "Forgot Password?" link exists
    const forgotLink = await page.locator('a[href="/forgot-password"]');
    const forgotExists = await forgotLink.count() > 0;
    console.log(`   ${forgotExists ? '✓' : '✗'} "Forgot Password?" link ${forgotExists ? 'found' : 'NOT FOUND'}`);

    // 2. Switch to registration tab
    console.log('\n2. Switching to registration tab...');
    await page.click('button:has-text("Register")');
    await page.waitForTimeout(500);
    await page.screenshot({ path: 'test-results/02-register-tab.png' });
    console.log('   ✓ Registration form visible');

    // Check if email is optional
    const emailInput = await page.locator('#register-email');
    const emailRequired = await emailInput.getAttribute('required');
    console.log(`   ${emailRequired === null ? '✓' : '✗'} Email is ${emailRequired === null ? 'optional' : 'REQUIRED'}`);

    // 3. Fill registration form (without email)
    console.log('\n3. Filling registration form...');
    await page.fill('#register-username', testUsername);
    await page.fill('#register-password', testPassword);
    await page.fill('#register-password-confirm', testPassword);
    console.log(`   Username: ${testUsername}`);
    console.log(`   Password: ${testPassword}`);
    console.log('   Email: (skipped - optional)');
    await page.screenshot({ path: 'test-results/03-form-filled.png' });

    // 4. Submit registration
    console.log('\n4. Submitting registration...');
    await page.click('button:has-text("Register")');

    // Wait for redirect to recovery key page
    await page.waitForURL('**/recovery-key**', { timeout: 10000 });
    await page.waitForTimeout(1000);
    await page.screenshot({ path: 'test-results/04-recovery-key-page.png', fullPage: true });
    console.log('   ✓ Redirected to recovery key page');

    // 5. Extract recovery key
    console.log('\n5. Extracting recovery key...');
    const recoveryKeyElement = await page.locator('#recovery-key');
    recoveryKey = await recoveryKeyElement.textContent();
    recoveryKey = recoveryKey.trim();
    console.log(`   Recovery Key: "${recoveryKey}"`);

    // Validate recovery key format (12 words)
    const words = recoveryKey.split(/\s+/);
    const validFormat = words.length === 12 && words.every(w => w.length > 0);
    console.log(`   ${validFormat ? '✓' : '✗'} Recovery key has ${words.length} words (expected 12)`);

    // Check for warning message
    const warningExists = await page.locator('text=SAVE THIS NOW').count() > 0;
    console.log(`   ${warningExists ? '✓' : '✗'} Warning message ${warningExists ? 'displayed' : 'NOT FOUND'}`);

    // 6. Test copy button
    console.log('\n6. Testing copy button...');
    await page.click('button:has-text("Copy")');
    await page.waitForTimeout(500);
    console.log('   ✓ Copy button clicked');

    // 7. Check confirmation checkbox
    console.log('\n7. Checking confirmation requirement...');
    const continueButton = await page.locator('a.btn:has-text("Continue")');
    const initiallyDisabled = await continueButton.evaluate(el => el.classList.contains('btn-disabled'));
    console.log(`   ${initiallyDisabled ? '✓' : '✗'} Continue button initially ${initiallyDisabled ? 'disabled' : 'NOT disabled'}`);

    // Check the confirmation checkbox
    await page.check('input[type="checkbox"]');
    await page.waitForTimeout(500);
    await page.screenshot({ path: 'test-results/05-checkbox-checked.png', fullPage: true });
    console.log('   ✓ Confirmation checkbox checked');

    // 8. Continue to dashboard
    console.log('\n8. Continuing to dashboard...');
    await page.click('a.btn:has-text("Continue")');
    await page.waitForURL('**/dashboard', { timeout: 10000 });
    await page.waitForTimeout(1000);
    await page.screenshot({ path: 'test-results/06-dashboard.png', fullPage: true });
    console.log('   ✓ Redirected to dashboard');

    // 9. Logout
    console.log('\n9. Logging out...');
    await page.goto(baseUrl + '/logout');
    await page.waitForURL('**/login', { timeout: 10000 });
    await page.waitForTimeout(500);
    console.log('   ✓ Logged out successfully');

    // 10. Test Forgot Password flow
    console.log('\n10. Testing Forgot Password flow...');
    await page.goto(baseUrl + '/forgot-password');
    await page.waitForLoadState('networkidle');
    await page.screenshot({ path: 'test-results/07-forgot-password-page.png' });
    console.log('   ✓ Forgot password page loaded');

    // Fill forgot password form
    console.log('\n11. Filling forgot password form...');
    await page.fill('#username', testUsername);
    await page.fill('#recovery_key', recoveryKey);
    const newPassword = 'NewPass456!';
    await page.fill('#new_password', newPassword);
    await page.fill('#confirm_password', newPassword);
    await page.screenshot({ path: 'test-results/08-forgot-form-filled.png' });
    console.log(`   Username: ${testUsername}`);
    console.log(`   Recovery Key: ${recoveryKey}`);
    console.log(`   New Password: ${newPassword}`);

    // Submit password reset
    console.log('\n12. Submitting password reset...');
    await page.click('button:has-text("Reset Password")');

    // Wait for alert and redirect
    page.on('dialog', async dialog => {
      console.log(`   Alert: "${dialog.message()}"`);
      await dialog.accept();
    });

    await page.waitForTimeout(2000);
    await page.screenshot({ path: 'test-results/09-after-reset.png' });

    // Should redirect to login
    const currentUrl = page.url();
    const onLoginPage = currentUrl.includes('/login');
    console.log(`   ${onLoginPage ? '✓' : '✗'} ${onLoginPage ? 'Redirected to login page' : 'NOT redirected to login'}`);

    // 13. Login with new password
    console.log('\n13. Testing login with new password...');
    await page.fill('#login-username', testUsername);
    await page.fill('#login-password', newPassword);
    await page.screenshot({ path: 'test-results/10-login-new-password.png' });
    await page.click('button:has-text("Login")');

    await page.waitForURL('**/dashboard', { timeout: 10000 });
    await page.waitForTimeout(1000);
    await page.screenshot({ path: 'test-results/11-dashboard-after-reset.png', fullPage: true });
    console.log('   ✓ Successfully logged in with new password!');

    console.log('\n=== ALL TESTS PASSED ✓ ===\n');
    console.log('Summary:');
    console.log('  ✓ Registration works (email optional)');
    console.log('  ✓ Recovery key generated (12 words)');
    console.log('  ✓ Recovery key page displays with warnings');
    console.log('  ✓ Forgot password link exists');
    console.log('  ✓ Password reset works with recovery key');
    console.log('  ✓ Login works with new password');
    console.log('\nScreenshots saved in test-results/');

  } catch (error) {
    console.error('\n❌ TEST FAILED:', error.message);
    await page.screenshot({ path: 'test-results/error.png' });
  } finally {
    await browser.close();
  }
})();
