// Copyright (c) Jeevanandam M. (https://github.com/jeevatkm)
// go-aah/aah source code and usage is governed by a MIT style
// license that can be found in the LICENSE file.

package aah

import (
	"fmt"

	"aahframework.org/ahttp.v0"
	"aahframework.org/config.v0"
	"aahframework.org/essentials.v0"
	"aahframework.org/log.v0"
	"aahframework.org/security.v0-unstable"
	"aahframework.org/security.v0-unstable/authc"
	"aahframework.org/security.v0-unstable/scheme"
	"aahframework.org/security.v0-unstable/session"
)

const (
	keyAuthcInfo    = "_aahAuthcInfo"
	keySubjectValue = "_aahSubject"
)

var appSecurityManager = security.New()

//‾‾‾‾‾‾‾‾‾‾‾‾‾‾‾‾‾‾‾‾‾‾‾‾‾‾‾‾‾‾‾‾‾‾‾
// Global methods
//___________________________________

// AppSecurityManager method returns the application security instance,
// which manages the Session, CORS, CSRF, Security Headers, etc.
func AppSecurityManager() *security.Manager {
	return appSecurityManager
}

// AppSessionManager method returns the application session manager.
// By default session is stateless.
func AppSessionManager() *session.Manager {
	return AppSecurityManager().SessionManager
}

// AddSessionStore method allows you to add custom session store which
// implements `session.Storer` interface. The `name` parameter is used in
// aah.conf on `session.store.type = "name"`.
func AddSessionStore(name string, store session.Storer) error {
	return session.AddStore(name, store)
}

//‾‾‾‾‾‾‾‾‾‾‾‾‾‾‾‾‾‾‾‾‾‾‾‾‾‾‾‾‾‾‾‾‾‾‾‾‾‾‾‾‾‾
// Authentication and Authorization methods
//__________________________________________

func (e engine) handleAuthcAndAuthz(ctx *Context) flowResult {
	// If route auth is `anonymous` then continue the request flow
	// No authentication or authorization is required for that route.
	if ctx.route.Auth == "anonymous" {
		log.Debugf("Route auth is anonymous: %v", ctx.Req.Path)
		return flowCont
	}

	authScheme := AppSecurityManager().GetAuthScheme(ctx.route.Auth)
	if authScheme == nil {
		// If auth scheme is nil then treat it as `anonymous`.
		log.Tracef("Route auth scheme is nil, treat it as anonymous: %v", ctx.Req.Path)
		return flowCont
	}

	log.Debugf("Route auth scheme: %s", authScheme.Scheme())
	switch authScheme.Scheme() {
	case "form":
		return e.doFormAuthcAndAuthz(authScheme, ctx)
	default:
		return e.doAuthcAndAuthz(authScheme, ctx)
	}
}

// doFormAuthcAndAuthz method does Form Authentication and Authorization.
func (e *engine) doFormAuthcAndAuthz(ascheme scheme.Schemer, ctx *Context) flowResult {
	formAuth := ascheme.(*scheme.FormAuth)

	// In Form authentication check session is already authentication if yes
	// then continue the request flow immediately.
	if ctx.Subject().IsAuthenticated() {
		if ctx.Session().IsKeyExists(keyAuthcInfo) {
			ctx.Subject().AuthenticationInfo = ctx.Session().Get(keyAuthcInfo).(*authc.AuthenticationInfo)

			// TODO cache for AuthorizationInfo
			ctx.Subject().AuthorizationInfo = formAuth.DoAuthorizationInfo(ctx.Subject().AuthenticationInfo)
		} else {
			log.Warn("It seems there is an issue with session data of AuthenticationInfo")
		}

		return flowCont
	}

	// Check route is login submit URL otherwise send it login URL.
	// Since session is not authenticated.
	if formAuth.LoginSubmitURL != ctx.route.Path && ctx.Req.Method != ahttp.MethodPost {
		loginURL := formAuth.LoginURL
		if formAuth.LoginURL != ctx.Req.Path {
			loginURL = fmt.Sprintf("%s?_rt=%s", loginURL, ctx.Req.Raw.RequestURI)
		}
		ctx.Reply().Redirect(loginURL)
		e.writeReply(ctx)
		return flowStop
	}

	publishOnPreAuthEvent(ctx)

	// Do Authentication
	authcInfo, err := formAuth.DoAuthenticate(formAuth.ExtractAuthenticationToken(ctx.Req))
	if err == authc.ErrAuthenticationFailed {
		log.Infof("Authentication is failed, sending to login failure URL")
		ctx.Reply().Redirect(formAuth.LoginFailureURL + "&_rt=" + ctx.Req.Raw.FormValue("_rt"))
		e.writeReply(ctx)
		return flowStop
	}

	ctx.Subject().AuthenticationInfo = authcInfo
	ctx.Subject().AuthorizationInfo = formAuth.DoAuthorizationInfo(authcInfo)
	ctx.Session().IsAuthenticated = true

	// Remove the credential
	ctx.Subject().AuthenticationInfo.Credential = nil
	ctx.Session().Set(keyAuthcInfo, ctx.Subject().AuthenticationInfo)

	publishOnPostAuthEvent(ctx)

	rt := ctx.Req.Raw.FormValue("_rt")
	if formAuth.IsAlwaysToDefaultTarget || ess.IsStrEmpty(rt) {
		ctx.Reply().Redirect(formAuth.DefaultTargetURL)
	} else {
		log.Debugf("Redirect to URL found: %v", rt)
		ctx.Reply().Redirect(rt)
	}

	e.writeReply(ctx)
	return flowStop
}

// doAuthcAndAuthz method does Authentication and Authorization.
func (e *engine) doAuthcAndAuthz(ascheme scheme.Schemer, ctx *Context) flowResult {
	publishOnPreAuthEvent(ctx)

	// Do Authentication
	authcInfo, err := ascheme.DoAuthenticate(ascheme.ExtractAuthenticationToken(ctx.Req))
	if err == authc.ErrAuthenticationFailed {
		log.Infof("Authentication is failed")
		ctx.Reply().Unauthorized()

		if ascheme.Scheme() == "basic" {
			basicAuth := ascheme.(*scheme.BasicAuth)
			ctx.Reply().Header(ahttp.HeaderWWWAuthenticate, `Basic realm="`+basicAuth.RealmName+`"`)
		}
		// TODO write response based on Content type
		e.writeReply(ctx)
		return flowStop
	}

	ctx.Subject().AuthenticationInfo = authcInfo
	ctx.Subject().AuthorizationInfo = ascheme.DoAuthorizationInfo(authcInfo)
	ctx.Session().IsAuthenticated = true

	// Remove the credential
	ctx.Subject().AuthenticationInfo.Credential = nil

	publishOnPostAuthEvent(ctx)

	return flowCont
}

//‾‾‾‾‾‾‾‾‾‾‾‾‾‾‾‾‾‾‾‾‾‾‾‾‾‾‾‾‾‾‾‾‾‾‾
// Unexported methods
//___________________________________

func initSecurity(appCfg *config.Config) error {
	if err := appSecurityManager.Init(appCfg); err != nil {
		return err
	}

	// Based on aah server SSL configuration `http.Cookie.Secure` value is set, even
	// though it's true in aah.conf at `security.session.secure = true`.
	if AppSessionManager() != nil {
		AppSessionManager().Options.Secure = AppIsSSLEnabled()
	}

	return nil
}

func isFormAuthLoginRoute(ctx *Context) bool {
	authScheme := AppSecurityManager().GetAuthScheme(ctx.route.Auth)
	if authScheme != nil && authScheme.Scheme() == "form" {
		return authScheme.(*scheme.FormAuth).LoginSubmitURL == ctx.route.Path
	}
	return false
}

//‾‾‾‾‾‾‾‾‾‾‾‾‾‾‾‾‾‾‾‾‾‾‾‾‾‾‾‾‾‾‾‾‾‾‾
// Template methods
//___________________________________

// tmplSessionValue method returns session value for the given key. If session
// object unavailable this method returns nil.
func tmplSessionValue(viewArgs map[string]interface{}, key string) interface{} {
	if sub := getSubjectFromViewArgs(viewArgs); sub != nil {
		if sub.Session != nil {
			value := sub.Session.Get(key)
			return sanatizeValue(value)
		}
	}
	return nil
}

// tmplFlashValue method returns session value for the given key. If session
// object unavailable this method returns nil.
func tmplFlashValue(viewArgs map[string]interface{}, key string) interface{} {
	if sub := getSubjectFromViewArgs(viewArgs); sub != nil {
		if sub.Session != nil {
			value := sub.Session.GetFlash(key)
			return sanatizeValue(value)
		}
	}
	return nil
}

// tmplIsAuthenticated method returns the value of `Session.IsAuthenticated`.
func tmplIsAuthenticated(viewArgs map[string]interface{}) interface{} {
	if sub := getSubjectFromViewArgs(viewArgs); sub != nil {
		if sub.Session != nil {
			return sub.Session.IsAuthenticated
		}
	}
	return false
}

func getSubjectFromViewArgs(viewArgs map[string]interface{}) *security.Subject {
	if sv, found := viewArgs[keySubjectValue]; found {
		return sv.(*security.Subject)
	}
	return nil
}
