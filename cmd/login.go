package cmd

import (
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/badoux/checkmail"
	"github.com/bgentry/speakeasy"
	"github.com/kataras/golog"
	"github.com/mitchellh/go-wordwrap"
	"github.com/mxschmitt/playwright-go"
	"github.com/spf13/cobra"

	"github.com/AlbinoGeek/sc2-rsu/sc2replaystats"
)

var (
	loginWarning = "We are about to login to sc2replaystats for you to obtain or generate your API key. We will have to ask you for your password, which we WILL NOT SAVE. If you want to avoid providing your account password please call this command with your API key instead."

	loginCmd = &cobra.Command{
		Use: "login <apikey or email>",
		Args: func(cmd *cobra.Command, args []string) error {
			if l := len(args); l != 1 {
				return fmt.Errorf("wrong argument count: expected 1, got %d", l)
			}

			// is it an API key?
			if sc2replaystats.ValidAPIKey(args[0]) {
				return nil
			}

			// is it an email address?
			if err := checkmail.ValidateFormat(args[0]); err != nil {
				return fmt.Errorf("email address: %v", err)
			}

			return nil
		},
		Short: "Add an sc2replaystats account to the config file",
		RunE: func(cmd *cobra.Command, args []string) error {
			// is it an API key?
			if sc2replaystats.ValidAPIKey(args[0]) {
				return setAPIkey(args[0])
			}

			// is it an email address?
			line := strings.Repeat("=", termWidth/2)
			fmt.Printf(
				"\n%s\n%s\nExample: %s login <apikey>\n%s\n\n",
				line,
				wordwrap.WrapString(loginWarning, uint(termWidth/2)),
				os.Args[0],
				line)

			password, err := speakeasy.Ask(fmt.Sprintf("Password for sc2ReplayStats account %s: ", args[0]))
			if err != nil {
				return fmt.Errorf("failed to prompt user for password: %v", err)
			}

			golog.Debug("Setting up browser...")
			t := time.Now()
			pw, browser, page, err := newBrowser()
			if pw != nil {
				defer pw.Stop()
			}
			if browser != nil {
				defer browser.Close()
			}
			if page != nil {
				defer page.Close()
			}
			if err != nil {
				return fmt.Errorf("failed to setup browser: %v", err)
			}
			golog.Debugf("browser setup took %s", time.Since(t))

			t = time.Now()
			accid, err := login(page, args[0], password)
			if err != nil {
				return fmt.Errorf("email login error: %v", err)
			}
			golog.Debugf("login took %s", time.Since(t))
			golog.Infof("Success! Logged in to account #%v", accid)

			t = time.Now()
			key, err := extractAPIKey(page, accid)
			if err != nil {
				return fmt.Errorf("extractAPIKey error: %v", err)
			}
			golog.Debugf("extractAPIKey took %s", time.Since(t))
			if err = setAPIkey(key); err != nil {
				return fmt.Errorf("setAPIKey: %v", err)
			}
			return nil
		},
	}
)

func extractAPIKey(page *playwright.Page, accountID string) (string, error) {
	if _, err := page.Goto(fmt.Sprintf("%s/account/settings/%v", sc2replaystats.WebRoot, accountID)); err != nil {
		return "", fmt.Errorf("failed to navigate to settings page: %v", err)
	}

	golog.Debug("Waiting for settings page to load...")

	e, err := page.WaitForSelector("*css=a[data-toggle='tab'] >> text=API Access")
	if err != nil {
		return "", fmt.Errorf("[settings] failed to locate API Access section: %v", err)
	}

	golog.Debug("Clicking 'API Access'...")

	if err = e.Click(); err != nil {
		return "", fmt.Errorf("[settings] failed to click API Access: %v", err)
	}

	golog.Debug("Finding API key...")

	e, err = page.QuerySelector("*css=.form-group >> text=Authorization Key")

	if e == nil || err != nil {
		golog.Info("Generating new API key...")

		e, err = page.QuerySelector("text=Generate New API Key")

		if err != nil {
			return "", fmt.Errorf("[settings] failed to locate Generate New API Key button: %v", err)
		}

		if err = e.Click(); err != nil {
			return "", fmt.Errorf("[settings] failed to click Generate New API Key button: %v", err)
		}

		e, err = page.WaitForSelector("*css=.form-group >> text=Authorization Key")
	}

	if err != nil || e == nil {
		return "", fmt.Errorf("[settings] failed to locate \"Authorization Key\" (API Key): %v", err)
	}

	t, err := e.InnerText()

	if err != nil {
		return "", fmt.Errorf("[settings] failed to resolve \"Authorization Key\" (API Key) Text: %v", err)
	}

	return strings.Trim(strings.Split(t, ": ")[1], " \r\n\t"), nil
}

func login(page *playwright.Page, email string, password string) (accountID string, err error) {
	golog.Debug("Navigating to login page...")

	if _, err := page.Goto(fmt.Sprintf("%s/Account/signin", sc2replaystats.WebRoot)); err != nil {
		return "", fmt.Errorf("failed to navigate to signin page: %v", err)
	}

	golog.Debug("Filling login form...")

	input, err := page.QuerySelector("css=input[name='email']")

	if err != nil || input == nil {
		return "", fmt.Errorf("[signin] failed to locate email field: %v", err)
	}

	if err = input.Fill(email); err != nil {
		return "", fmt.Errorf("[signin] failed to fill email field: %v", err)
	}

	if input, err = page.QuerySelector("css=input[name='password']"); err != nil || input == nil {
		return "", fmt.Errorf("[signin] failed to locate password field: %v", err)
	}

	if err = input.Fill(password); err != nil {
		return "", fmt.Errorf("[signin] failed to fill password field: %v", err)
	}

	golog.Debugf("Submitting login form...")

	if input, err = page.QuerySelector("css=input[value='Sign In']"); err != nil || input == nil {
		return "", fmt.Errorf("[signin] failed to locate submit button: %v", err)
	}

	if err = input.Click(); err != nil {
		return "", fmt.Errorf("[signin] failed to click submit button: %v", err)
	}

	url := page.URL()

	if !strings.Contains(url, "display") {
		if alert, err := page.QuerySelector("css=.alert-danger"); err == nil && alert != nil {
			if text, err := alert.InnerText(); err == nil {
				return "", fmt.Errorf("[signin] login failed, sc2replaystats says: %v", text)
			}
		}

		return "", fmt.Errorf("[signin] unexpected redirect URL, login probably failed: %v", url)
	}

	parts := strings.Split(url, "/")

	return strings.Split(parts[len(parts)-1], "#")[0], nil
}

func newBrowser() (pw *playwright.Playwright, browser *playwright.Browser, page *playwright.Page, err error) {
	if pw, err = playwright.Run(); err == nil {
		if browser, err = pw.Chromium.Launch(); err == nil {
			page, err = browser.NewPage()
		}
	}

	return pw, browser, page, err
}
