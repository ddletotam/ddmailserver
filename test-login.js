const { chromium } = require('playwright');

(async () => {
  const browser = await chromium.launch({ headless: false });
  const context = await browser.newContext();
  const page = await context.newPage();

  let apiCalled = false;

  page.on('request', request => {
    if (request.url().includes('/api/login')) {
      apiCalled = true;
      console.log(`\n>>> API LOGIN REQUEST`);
      console.log(`    Data: ${request.postData()}`);
    }
  });

  page.on('response', async response => {
    if (response.url().includes('/api/login')) {
      console.log(`\n<<< API LOGIN RESPONSE: ${response.status()}`);
      const text = await response.text();
      console.log(`    Body: ${text}`);
    }
  });

  try {
    console.log('=== Testing Login Form ===\n');

    await page.goto('http://jwebhelp.ru:8080/login');
    await page.waitForLoadState('networkidle');

    console.log('1. Filling login form...');
    await page.fill('#login-username', 'nonexistent_user');
    await page.fill('#login-password', 'wrongpassword');

    console.log('2. Submitting login form...');
    await page.click('button:has-text("Login")');

    await page.waitForTimeout(3000);

    console.log(`\n3. API called: ${apiCalled ? 'YES ✓' : 'NO ✗'}`);
    console.log(`   Current URL: ${page.url()}`);

    await page.screenshot({ path: 'test-results/login-test.png' });

  } catch (error) {
    console.error('Error:', error.message);
  } finally {
    await browser.close();
  }
})();
