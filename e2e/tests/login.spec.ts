import { E2E_PASSWORD, E2E_USERNAME, expect, loginAs, test } from '../fixtures/panel';

test.describe('login', () => {
  test('wrong password shows an error and stays on the login page', async ({ page }) => {
    await loginAs(page, E2E_USERNAME, 'definitely-not-the-password');

    await expect(page.getByText(/Invalid username or password/i)).toBeVisible();
    await expect(page).not.toHaveURL(/\/panel\//);
  });

  test('right password lands on the panel index', async ({ page }) => {
    await loginAs(page, E2E_USERNAME, E2E_PASSWORD);

    await expect(page).toHaveURL(/\/panel\/$/);
  });
});
