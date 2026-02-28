const { chromium } = require('playwright');

(async () => {
  const browser = await chromium.launch({ headless: false });
  const context = await browser.newContext();
  const page = await context.newPage();

  try {
    console.log('=== Testing Settings Page ===\n');

    // First, need to login
    console.log('1. Logging in...');
    await page.goto('http://jwebhelp.ru:8080/login');
    await page.waitForLoadState('networkidle');
    await page.waitForTimeout(1000);

    // Use existing test user
    await page.fill('#login-username', 'testuser_1772281854783');
    await page.fill('#login-password', 'NewPass456!');
    await page.locator('#login-form button[type="submit"]').click();

    await page.waitForURL('**/dashboard', { timeout: 10000 });
    console.log('   ✓ Logged in successfully');

    // Navigate to settings page
    console.log('\n2. Opening settings page...');
    await page.goto('http://jwebhelp.ru:8080/settings');
    await page.waitForLoadState('networkidle');
    await page.waitForTimeout(1000);

    // Take screenshot
    await page.screenshot({ path: 'test-results/settings-page.png', fullPage: true });

    // Check if page loaded correctly
    const pageTitle = await page.title();
    console.log(`   Page title: "${pageTitle}"`);

    // Check if settings sections are visible
    const userInfoVisible = await page.locator('text=User Information').isVisible();
    const passwordVisible = await page.locator('text=Change Password').isVisible();
    const languageVisible = await page.locator('text=Language').isVisible();
    const dangerZoneVisible = await page.locator('text=Danger Zone').isVisible();

    console.log(`   User Info section: ${userInfoVisible ? '✓ YES' : '✗ NO'}`);
    console.log(`   Change Password section: ${passwordVisible ? '✓ YES' : '✗ NO'}`);
    console.log(`   Language section: ${languageVisible ? '✓ YES' : '✗ NO'}`);
    console.log(`   Danger Zone section: ${dangerZoneVisible ? '✓ YES' : '✗ NO'}`);

    // Check if username is displayed
    const username = await page.locator('.info-item:has-text("Username") p').textContent();
    console.log(`   Username displayed: ${username}`);

    // Check if forms are present
    const passwordForm = await page.locator('form[hx-post="/api/settings/password"]').isVisible();
    const languageForm = await page.locator('form[hx-post="/api/settings/language"]').isVisible();

    console.log(`   Password form: ${passwordForm ? '✓ YES' : '✗ NO'}`);
    console.log(`   Language form: ${languageForm ? '✓ YES' : '✗ NO'}`);

    console.log('\n✓ Settings page loaded successfully');
    console.log('Screenshot saved to: test-results/settings-page.png');

  } catch (error) {
    console.error('\n❌ TEST FAILED:', error.message);
    await page.screenshot({ path: 'test-results/settings-error.png' });
  } finally {
    await browser.close();
  }
})();
