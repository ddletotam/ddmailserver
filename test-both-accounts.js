const { chromium } = require('playwright');

(async () => {
  const browser = await chromium.launch({ headless: false });
  const context = await browser.newContext();
  const page = await context.newPage();

  try {
    console.log('=== Testing with Real Email Accounts ===\n');

    const username = `testuser_${Date.now()}`;
    const password = 'TestPass123!';

    // 1. Register
    console.log('1. Registering user...');
    await page.goto('https://ddm.logdoc.ru/login');
    await page.click('button:has-text("Register")');
    await page.waitForTimeout(500);
    await page.fill('#register-username', username);
    await page.fill('#register-password', password);
    await page.fill('#register-password-confirm', password);
    await page.locator('#register-form button[type="submit"]').click();
    await page.waitForURL('**/recovery-key**', { timeout: 10000 });
    console.log('   ✓ Registered');

    await page.locator('input[type="checkbox"][x-model="confirmed"]').check();
    await page.waitForTimeout(500);
    await page.click('a:has-text("Continue to Dashboard")');
    await page.waitForURL('**/dashboard', { timeout: 10000 });

    // 2. Add first account (i2lab)
    console.log('\n2. Adding i2lab account...');
    await page.goto('https://ddm.logdoc.ru/accounts/new');
    await page.waitForLoadState('networkidle');

    await page.fill('#name', 'i2lab Mail');
    await page.fill('#email', 'lucky@i2lab.ru');
    await page.fill('#imap_host', 'mail.i2lab.ru');
    await page.fill('#imap_port', '993');
    await page.fill('#imap_username', 'lucky@i2lab.ru');
    await page.fill('#imap_password', 'eqlGKVNm9jKC5qXY');
    await page.fill('#smtp_host', 'mail.i2lab.ru');
    await page.fill('#smtp_port', '587');
    await page.fill('#smtp_username', 'lucky@i2lab.ru');
    await page.fill('#smtp_password', 'eqlGKVNm9jKC5qXY');

    await page.locator('form.account-form button[type="submit"]').click();
    await page.waitForTimeout(3000);
    console.log('   ✓ Submitted i2lab account');

    // 3. Add second account (Yandex)
    console.log('\n3. Adding Yandex account...');
    await page.goto('https://ddm.logdoc.ru/accounts/new');
    await page.waitForLoadState('networkidle');

    await page.fill('#name', 'Yandex Mail');
    await page.fill('#email', 'deniskr@yandex.ru');
    await page.fill('#imap_host', 'imap.yandex.ru');
    await page.fill('#imap_port', '993');
    await page.fill('#imap_username', 'deniskr@yandex.ru');
    await page.fill('#imap_password', 'uklpwndiqxdaxvgo');
    await page.fill('#smtp_host', 'smtp.yandex.ru');
    await page.fill('#smtp_port', '587');
    await page.fill('#smtp_username', 'deniskr@yandex.ru');
    await page.fill('#smtp_password', 'uklpwndiqxdaxvgo');

    await page.locator('form.account-form button[type="submit"]').click();
    await page.waitForTimeout(3000);
    console.log('   ✓ Submitted Yandex account');

    // 4. Check accounts page
    console.log('\n4. Checking accounts list...');
    await page.goto('https://ddm.logdoc.ru/accounts');
    await page.waitForLoadState('networkidle');
    await page.waitForTimeout(2000);

    const i2labVisible = await page.locator('text=i2lab Mail').isVisible().catch(() => false);
    const yandexVisible = await page.locator('text=Yandex Mail').isVisible().catch(() => false);

    console.log(`   i2lab account: ${i2labVisible ? '✓ VISIBLE' : '✗ NOT FOUND'}`);
    console.log(`   Yandex account: ${yandexVisible ? '✓ VISIBLE' : '✗ NOT FOUND'}`);

    await page.screenshot({ path: 'test-results/both-accounts-list.png', fullPage: true });

    // 5. Check inbox
    console.log('\n5. Checking inbox...');
    await page.goto('https://ddm.logdoc.ru/inbox');
    await page.waitForLoadState('networkidle');
    await page.waitForTimeout(5000); // Give time for sync

    const messageCount = await page.locator('.message-item').count().catch(() => 0);
    console.log(`   Messages found: ${messageCount}`);

    await page.screenshot({ path: 'test-results/both-accounts-inbox.png', fullPage: true });

    // 6. Wait for manual inspection
    console.log('\n✅ Test completed!');
    console.log(`User: ${username} / ${password}`);
    console.log('\nBrowser will stay open for inspection...');
    await page.waitForTimeout(60000);

  } catch (error) {
    console.error('\n❌ ERROR:', error.message);
    await page.screenshot({ path: 'test-results/both-accounts-error.png' });
  } finally {
    await browser.close();
  }
})();
