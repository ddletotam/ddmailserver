const { chromium } = require('playwright');

(async () => {
  const browser = await chromium.launch({ headless: false });
  const context = await browser.newContext();
  const page = await context.newPage();

  try {
    console.log('=== Testing with Real Email Account ===\n');

    const username = `testuser_${Date.now()}`;
    const password = 'TestPass123!';

    // 1. Register new user
    console.log('1. Registering user...');
    await page.goto('https://mail.letotam.ru/login');
    await page.waitForLoadState('networkidle');

    await page.click('button:has-text("Register")');
    await page.waitForTimeout(500);

    await page.fill('#register-username', username);
    await page.fill('#register-password', password);
    await page.fill('#register-password-confirm', password);
    await page.locator('#register-form button[type="submit"]').click();

    await page.waitForURL('**/recovery-key**', { timeout: 10000 });
    console.log('   ✓ User registered');

    await page.locator('input[type="checkbox"][x-model="confirmed"]').check();
    await page.waitForTimeout(500);
    await page.click('a:has-text("Continue to Dashboard")');
    await page.waitForURL('**/dashboard', { timeout: 10000 });

    // 2. Add real email account
    console.log('\n2. Adding email account...');
    await page.goto('https://mail.letotam.ru/accounts/new');
    await page.waitForLoadState('networkidle');
    await page.waitForTimeout(1000);

    // Fill account details
    await page.fill('#name', 'i2lab Mail Account');
    await page.fill('#email', 'lucky@i2lab.ru');

    // IMAP settings
    await page.fill('#imap_host', 'mail.i2lab.ru');
    await page.fill('#imap_port', '993');
    await page.fill('#imap_username', 'lucky@i2lab.ru');
    await page.fill('#imap_password', 'eqlGKVNm9jKC5qXY');
    // TLS should be checked by default

    // SMTP settings
    await page.fill('#smtp_host', 'mail.i2lab.ru');
    await page.fill('#smtp_port', '587');
    await page.fill('#smtp_username', 'lucky@i2lab.ru');
    await page.fill('#smtp_password', 'eqlGKVNm9jKC5qXY');

    await page.screenshot({ path: 'test-results/real-account-form.png', fullPage: true });

    // Submit form
    await page.locator('form.account-form button[type="submit"]').click();
    await page.waitForTimeout(3000);

    await page.screenshot({ path: 'test-results/real-account-added.png', fullPage: true });
    console.log('   ✓ Account form submitted');

    // 3. Check accounts page
    console.log('\n3. Checking accounts page...');
    await page.goto('https://mail.letotam.ru/accounts');
    await page.waitForLoadState('networkidle');
    await page.waitForTimeout(2000);

    const accountVisible = await page.locator('text=i2lab Mail Account').isVisible().catch(() => false);
    console.log(`   Account visible: ${accountVisible ? '✓ YES' : '✗ NO'}`);

    await page.screenshot({ path: 'test-results/real-accounts-list.png', fullPage: true });

    // 4. Check inbox
    console.log('\n4. Checking inbox...');
    await page.goto('https://mail.letotam.ru/inbox');
    await page.waitForLoadState('networkidle');
    await page.waitForTimeout(3000); // Give time for sync

    const messagesLoading = await page.locator('text=Loading messages').isVisible().catch(() => false);
    const noMessages = await page.locator('text=No messages').isVisible().catch(() => false);
    const hasMessages = await page.locator('.message-item').count().catch(() => 0);

    console.log(`   Loading: ${messagesLoading ? 'YES' : 'NO'}`);
    console.log(`   No messages: ${noMessages ? 'YES' : 'NO'}`);
    console.log(`   Messages count: ${hasMessages}`);

    await page.screenshot({ path: 'test-results/real-inbox.png', fullPage: true });

    // 5. Try to refresh
    console.log('\n5. Testing refresh...');
    await page.click('button:has-text("Refresh")');
    await page.waitForTimeout(2000);

    await page.screenshot({ path: 'test-results/real-inbox-refreshed.png', fullPage: true });

    console.log('\n✅ Real account test completed!');
    console.log(`\nTest user: ${username} / ${password}`);
    console.log('Email account: lucky@i2lab.ru added');

  } catch (error) {
    console.error('\n❌ TEST FAILED:', error.message);
    await page.screenshot({ path: 'test-results/real-account-error.png' });
    console.error(error.stack);
  } finally {
    // Keep browser open for manual inspection
    console.log('\nBrowser will stay open for 30 seconds for inspection...');
    await page.waitForTimeout(30000);
    await browser.close();
  }
})();
