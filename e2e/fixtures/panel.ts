import { test as base, expect, type APIRequestContext, type Locator, type Page } from '@playwright/test';

/**
 * Credentials the panel is started with. `e2e/docker-compose.yml` passes
 * these into the container (E2E_USERNAME/E2E_PASSWORD), which the compose
 * entrypoint feeds to `x-ui setting -username -password` before the panel
 * starts. Kept in sync here via the same env vars so a spec run against a
 * panel started outside `make e2e` (E2E_BASE_URL pointed elsewhere) can
 * still authenticate by exporting the same variables.
 */
export const E2E_USERNAME = process.env.E2E_USERNAME || 'e2e';
export const E2E_PASSWORD = process.env.E2E_PASSWORD || 'e2e-password';

// The login form (web/html/login.html) is Ant Design Vue components, not
// plain HTML — <a-input>/<a-input-password> carry a prefix icon slot, which
// makes them render a wrapping span around the real <input>. Ant Design Vue
// forwards unrecognized attributes (like our data-testid) to that real
// <input> in the common case, but this selector also matches the wrapper
// so a spec never has to know which shape a given antd version produces.
// This is the one CSS this file uses, and it is still keyed by
// data-testid, never by class names or DOM position.
function byTestId(page: Page, testId: string): Locator {
  return page.locator(`input[data-testid="${testId}"], [data-testid="${testId}"] input`).first();
}

export function usernameField(page: Page): Locator {
  return byTestId(page, 'login-username');
}

export function passwordField(page: Page): Locator {
  return byTestId(page, 'login-password');
}

export function submitButton(page: Page): Locator {
  return page.getByTestId('login-submit');
}

/**
 * Fills and submits the real login form at `/`. Does not assert the
 * outcome — callers check for the panel redirect (success) or the error
 * toast (failure).
 */
export async function loginAs(page: Page, username: string, password: string): Promise<void> {
  await page.goto('/');
  await usernameField(page).fill(username);
  await passwordField(page).fill(password);
  await submitButton(page).click();
}

type PanelFixtures = {
  /** A `page` already authenticated through the real login form, resolved on /panel/. */
  authedPage: Page;
  /** An API request context sharing the authenticated session's storage state (cookies) with `authedPage`. */
  authedRequest: APIRequestContext;
};

export const test = base.extend<PanelFixtures>({
  authedPage: async ({ page }, use) => {
    await loginAs(page, E2E_USERNAME, E2E_PASSWORD);
    await expect(page).toHaveURL(/\/panel\/?$/);
    await use(page);
  },

  authedRequest: async ({ playwright, baseURL, authedPage }, use) => {
    const storageState = await authedPage.context().storageState();
    const context = await playwright.request.newContext({ baseURL, storageState });
    await use(context);
    await context.dispose();
  },
});

export { expect };
