const { chromium } = require('playwright');

(async () => {
  const browser = await chromium.launch({ headless: false });
  const context = await browser.newContext();
  const page = await context.newPage();

  const testUsername = `testuser_${Date.now()}`;
  const testPassword = 'TestPass123!';

  page.on('request', request => {
    if (request.url().includes('/api/register')) {
      console.log(`\n>>> POST /api/register`);
      console.log(`    Body: ${request.postData()}`);
    }
  });

  page.on('response', async response => {
    if (response.url().includes('/api/register')) {
      console.log(`\n<<< ${response.status()} /api/register`);
      try {
        const text = await response.text();
        console.log(`    Response: ${text}`);
      } catch (e) {}
    }
  });

  page.on('console', msg => {
    const text = msg.text();
    if (text.includes('htmx') || text.includes('Failed') || text.includes('Error') || text.includes('parse')) {
      console.log(`[Browser] ${text}`);
    }
  });

  try {
    console.log('=== Detailed Registration Test ===\n');

    await page.goto('http://jwebhelp.ru:8080/login');
    await page.waitForLoadState('networkidle');

    console.log('1. Switching to register tab...');
    await page.click('#register-tab');
    await page.waitForTimeout(1000);

    console.log('2. Filling form...');
    await page.fill('#register-username', testUsername);
    await page.fill('#register-password', testPassword);
    await page.fill('#register-password-confirm', testPassword);
    console.log(`   Username: ${testUsername}`);

    console.log('\n3. Submitting form...');
    await page.click('button[type="submit"]:visible');

    console.log('\n4. Waiting for response...');
    await page.waitForTimeout(5000);

    console.log(`\n5. Current URL: ${page.url()}`);

    // Check for error message
    const errorMsg = await page.locator('#register-error').textContent();
    if (errorMsg && errorMsg.trim()) {
      console.log(`   Error displayed: "${errorMsg}"`);
    }

    await page.screenshot({ path: 'test-results/detailed-registration.png' });

  } catch (error) {
    console.error('\nError:', error.message);
  } finally {
    await browser.close();
  }
})();
