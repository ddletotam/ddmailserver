const { chromium } = require('playwright');

(async () => {
  const browser = await chromium.launch({ headless: false });
  const context = await browser.newContext();
  const page = await context.newPage();

  const baseUrl = 'http://jwebhelp.ru:8080';
  const testUsername = `testuser_${Date.now()}`;
  const testPassword = 'TestPass123!';

  // Capture all network requests
  page.on('request', request => {
    if (request.url().includes('/api/')) {
      console.log(`>>> ${request.method()} ${request.url()}`);
      if (request.postData()) {
        console.log(`    Body: ${request.postData()}`);
      }
    }
  });

  page.on('response', async response => {
    if (response.url().includes('/api/')) {
      console.log(`<<< ${response.status()} ${response.url()}`);
      try {
        const body = await response.text();
        console.log(`    Response: ${body}`);
      } catch (e) {
        console.log(`    (Could not read response body)`);
      }
    }
  });

  page.on('console', msg => {
    console.log(`[Browser Console] ${msg.text()}`);
  });

  try {
    console.log('\n=== Testing Registration with Debug ===\n');

    // Navigate to login page
    console.log('1. Opening login page...');
    await page.goto(baseUrl + '/login');
    await page.waitForLoadState('networkidle');

    // Switch to registration tab
    console.log('\n2. Switching to registration tab...');
    await page.click('button:has-text("Register")');
    await page.waitForTimeout(500);

    // Fill registration form
    console.log('\n3. Filling registration form...');
    await page.fill('#register-username', testUsername);
    await page.fill('#register-password', testPassword);
    await page.fill('#register-password-confirm', testPassword);
    console.log(`   Username: ${testUsername}`);
    console.log(`   Password: ${testPassword}`);

    // Submit registration
    console.log('\n4. Submitting registration...');
    await page.click('button:has-text("Register")');

    // Wait to see what happens
    console.log('\n5. Waiting for response...');
    await page.waitForTimeout(5000);

    console.log('\n6. Current URL:', page.url());
    await page.screenshot({ path: 'test-results/debug-after-submit.png' });

    // Check for any error messages
    const errorMessage = await page.locator('.error-message').textContent();
    if (errorMessage && errorMessage.trim()) {
      console.log('   Error message:', errorMessage);
    }

  } catch (error) {
    console.error('\n❌ ERROR:', error.message);
    await page.screenshot({ path: 'test-results/debug-error.png' });
  } finally {
    await browser.close();
  }
})();
