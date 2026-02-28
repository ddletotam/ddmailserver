const { chromium } = require('playwright');

(async () => {
  const browser = await chromium.launch({ headless: false });
  const context = await browser.newContext();
  const page = await context.newPage();

  try {
    console.log('=== Testing Account Form Page ===\n');

    // First, need to login
    console.log('1. Logging in...');
    await page.goto('http://jwebhelp.ru:8080/login');
    await page.waitForLoadState('networkidle');
    await page.waitForTimeout(1000);

    // Use existing test user or create new one
    await page.fill('#login-username', 'testuser_1772281854783');
    await page.fill('#login-password', 'NewPass456!');
    await page.locator('#login-form button[type="submit"]').click();

    await page.waitForURL('**/dashboard', { timeout: 10000 });
    console.log('   ✓ Logged in successfully');

    // Navigate to account form
    console.log('\n2. Opening account form page...');
    await page.goto('http://jwebhelp.ru:8080/accounts/new');
    await page.waitForLoadState('networkidle');
    await page.waitForTimeout(1000);

    // Take screenshot
    await page.screenshot({ path: 'test-results/account-form-page.png', fullPage: true });

    // Check if styles are loaded
    const hasStyles = await page.evaluate(() => {
      const links = document.querySelectorAll('link[rel="stylesheet"]');
      return links.length > 0;
    });

    console.log(`   CSS loaded: ${hasStyles ? '✓ YES' : '✗ NO'}`);

    // Check if form is visible
    const formVisible = await page.locator('form.account-form').isVisible();
    console.log(`   Form visible: ${formVisible ? '✓ YES' : '✗ NO'}`);

    // Check for specific form elements
    const hasIMAPFields = await page.locator('#imap_host').isVisible();
    const hasSMTPFields = await page.locator('#smtp_host').isVisible();
    console.log(`   IMAP fields: ${hasIMAPFields ? '✓ YES' : '✗ NO'}`);
    console.log(`   SMTP fields: ${hasSMTPFields ? '✓ YES' : '✗ NO'}`);

    // Check page title
    const title = await page.title();
    console.log(`   Page title: "${title}"`);

    console.log('\n✓ Account form page loaded successfully');
    console.log('Screenshot saved to: test-results/account-form-page.png');

  } catch (error) {
    console.error('\n❌ TEST FAILED:', error.message);
    await page.screenshot({ path: 'test-results/account-form-error.png' });
  } finally {
    await browser.close();
  }
})();
