const { chromium } = require('playwright');

(async () => {
  const browser = await chromium.launch({ headless: false });
  const context = await browser.newContext();
  const page = await context.newPage();

  try {
    console.log('=== Testing Message View ===\n');

    // Login
    await page.goto('https://ddm.logdoc.ru/login');
    await page.fill('#login-username', 'testuser_1772288309706');
    await page.fill('#login-password', 'TestPass123!');
    await page.locator('#login-form button[type="submit"]').click();
    await page.waitForURL('**/dashboard', { timeout: 10000 });
    console.log('✓ Logged in');

    // Go to inbox
    await page.goto('https://ddm.logdoc.ru/inbox');
    await page.waitForLoadState('networkidle');
    await page.waitForTimeout(2000);

    const messageCount = await page.locator('.message-item').count();
    console.log(`\n📧 Messages in inbox: ${messageCount}`);

    if (messageCount === 0) {
      console.log('No messages to view');
      return;
    }

    // Click on first message
    console.log('\nOpening first message...');
    await page.locator('.message-item').first().click();
    await page.waitForLoadState('networkidle');
    await page.waitForTimeout(1000);

    // Check message view elements
    const subject = await page.locator('.message-subject').textContent();
    const from = await page.locator('.message-from-to .meta-row:has-text("From:") .meta-value').textContent();
    const hasBody = await page.locator('.message-body').isVisible();

    console.log(`\n✓ Message opened`);
    console.log(`  Subject: ${subject.trim()}`);
    console.log(`  From: ${from.trim()}`);
    console.log(`  Body visible: ${hasBody}`);

    // Check for attachments
    const attachmentsSection = await page.locator('.message-attachments').count();
    if (attachmentsSection > 0) {
      const attCount = await page.locator('.attachment-item').count();
      console.log(`  📎 Attachments: ${attCount}`);

      for (let i = 0; i < attCount; i++) {
        const attName = await page.locator('.attachment-item').nth(i).textContent();
        console.log(`    - ${attName.trim()}`);
      }
    } else {
      console.log(`  📎 Attachments: none`);
    }

    await page.screenshot({ path: 'test-results/message-view.png', fullPage: true });
    console.log('\n✅ Screenshot saved to test-results/message-view.png');

    // Test toolbar buttons
    const replyBtn = await page.locator('button:has-text("Reply")').isVisible();
    const forwardBtn = await page.locator('button:has-text("Forward")').isVisible();
    const deleteBtn = await page.locator('button:has-text("Delete")').isVisible();

    console.log(`\nToolbar buttons:`);
    console.log(`  Reply: ${replyBtn ? '✓' : '✗'}`);
    console.log(`  Forward: ${forwardBtn ? '✓' : '✗'}`);
    console.log(`  Delete: ${deleteBtn ? '✓' : '✗'}`);

    console.log('\n✅ Message view test completed!');
    await page.waitForTimeout(5000);

  } catch (error) {
    console.error('\n❌ Error:', error.message);
    await page.screenshot({ path: 'test-results/message-view-error.png' });
  } finally {
    await browser.close();
  }
})();
