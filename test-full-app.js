const { chromium } = require('playwright');

(async () => {
  const browser = await chromium.launch({ headless: false });
  const context = await browser.newContext();
  const page = await context.newPage();

  try {
    console.log('=== Testing Full Application ===\n');

    const username = `testuser_${Date.now()}`;
    const password = 'TestPass123!';

    // 1. Test Registration
    console.log('1. Testing Registration...');
    await page.goto('https://ddm.logdoc.ru/login');
    await page.waitForLoadState('networkidle');

    await page.click('button:has-text("Register")');
    await page.waitForTimeout(500);

    await page.fill('#register-username', username);
    await page.fill('#register-password', password);
    await page.fill('#register-password-confirm', password);
    await page.locator('#register-form button[type="submit"]').click();

    await page.waitForURL('**/recovery-key**', { timeout: 10000 });
    console.log('   ✓ Registration works');

    await page.screenshot({ path: 'test-results/full-01-recovery.png', fullPage: true });

    // 2. Continue to dashboard
    console.log('\n2. Testing Dashboard...');
    await page.locator('input[type="checkbox"][x-model="confirmed"]').check();
    await page.waitForTimeout(500);
    await page.click('a:has-text("Continue to Dashboard")');
    await page.waitForURL('**/dashboard', { timeout: 10000 });

    await page.screenshot({ path: 'test-results/full-02-dashboard.png', fullPage: true });
    console.log('   ✓ Dashboard loads');

    // 3. Test Accounts page
    console.log('\n3. Testing Accounts page...');
    await page.goto('https://ddm.logdoc.ru/accounts');
    await page.waitForLoadState('networkidle');
    await page.waitForTimeout(1000);

    const noAccounts = await page.locator('text=No email accounts').isVisible();
    console.log(`   Empty accounts list: ${noAccounts ? '✓ YES' : '✗ NO'}`);

    await page.screenshot({ path: 'test-results/full-03-accounts.png', fullPage: true });

    // 4. Test Account Form page
    console.log('\n4. Testing Account Form...');
    await page.goto('https://ddm.logdoc.ru/accounts/new');
    await page.waitForLoadState('networkidle');
    await page.waitForTimeout(1000);

    const formVisible = await page.locator('form.account-form').isVisible();
    const imapFields = await page.locator('#imap_host').isVisible();
    const smtpFields = await page.locator('#smtp_host').isVisible();

    console.log(`   Form visible: ${formVisible ? '✓ YES' : '✗ NO'}`);
    console.log(`   IMAP fields: ${imapFields ? '✓ YES' : '✗ NO'}`);
    console.log(`   SMTP fields: ${smtpFields ? '✓ YES' : '✗ NO'}`);

    await page.screenshot({ path: 'test-results/full-04-account-form.png', fullPage: true });

    // 5. Test Inbox page
    console.log('\n5. Testing Inbox...');
    await page.goto('https://ddm.logdoc.ru/inbox');
    await page.waitForLoadState('networkidle');
    await page.waitForTimeout(1000);

    const inboxEmpty = await page.locator('text=Your inbox is empty').isVisible();
    console.log(`   Empty inbox message: ${inboxEmpty ? '✓ YES' : '✗ NO'}`);

    await page.screenshot({ path: 'test-results/full-05-inbox.png', fullPage: true });

    // 6. Test Compose page
    console.log('\n6. Testing Compose...');
    await page.goto('https://ddm.logdoc.ru/compose');
    await page.waitForLoadState('networkidle');
    await page.waitForTimeout(1000);

    const composeForm = await page.locator('text=From Account').isVisible();
    console.log(`   Compose form: ${composeForm ? '✓ YES' : '✗ NO'}`);

    await page.screenshot({ path: 'test-results/full-06-compose.png', fullPage: true });

    // 7. Test Settings page
    console.log('\n7. Testing Settings...');
    await page.goto('https://ddm.logdoc.ru/settings');
    await page.waitForLoadState('networkidle');
    await page.waitForTimeout(1000);

    const userInfo = await page.locator('text=User Information').isVisible();
    console.log(`   Settings page: ${userInfo ? '✓ YES' : '✗ NO'}`);

    await page.screenshot({ path: 'test-results/full-07-settings.png', fullPage: true });

    // 8. Test Logout
    console.log('\n8. Testing Logout...');
    await page.goto('https://ddm.logdoc.ru/logout');
    await page.waitForURL('**/login', { timeout: 5000 });
    console.log('   ✓ Logout works');

    await page.screenshot({ path: 'test-results/full-08-logout.png', fullPage: true });

    // 9. Test Login
    console.log('\n9. Testing Login...');
    await page.fill('#login-username', username);
    await page.fill('#login-password', password);
    await page.locator('#login-form button[type="submit"]').click();

    await page.waitForURL('**/dashboard', { timeout: 10000 });
    console.log('   ✓ Login works');

    console.log('\n✅ All core features tested successfully!');
    console.log(`\nTest user: ${username} / ${password}`);

  } catch (error) {
    console.error('\n❌ TEST FAILED:', error.message);
    await page.screenshot({ path: 'test-results/full-error.png' });
  } finally {
    await browser.close();
  }
})();
