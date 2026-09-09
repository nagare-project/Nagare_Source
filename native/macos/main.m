#import <Cocoa/Cocoa.h>
#import <WebKit/WebKit.h>
#import <dlfcn.h>

typedef void *NagareNWEndpoint;
typedef void *NagareNWProxyConfiguration;

static void WriteJSON(NSDictionary *value) {
    NSData *data = [NSJSONSerialization dataWithJSONObject:value options:0 error:nil];
    if (!data) return;
    fwrite(data.bytes, 1, data.length, stdout);
    fwrite("\n", 1, 1, stdout);
    fflush(stdout);
}

static BOOL ConfigureProxy(WKWebsiteDataStore *store, NSString *host, NSString *port) {
    SEL setter = NSSelectorFromString(@"setProxyConfigurations:");
    if (![store respondsToSelector:setter]) return NO;

    void *network = dlopen("/System/Library/Frameworks/Network.framework/Network", RTLD_LAZY | RTLD_LOCAL);
    if (!network) network = dlopen("Network", RTLD_LAZY | RTLD_LOCAL);
    if (!network) return NO;

    NagareNWEndpoint (*createEndpoint)(const char *, const char *) = dlsym(network, "nw_endpoint_create_host");
    NagareNWProxyConfiguration (*createProxy)(NagareNWEndpoint, void *) = dlsym(network, "nw_proxy_config_create_http_connect");
    void (*setFailover)(NagareNWProxyConfiguration, BOOL) = dlsym(network, "nw_proxy_config_set_failover_allowed");
    if (!createEndpoint || !createProxy || !setFailover) return NO;

    NagareNWEndpoint endpoint = createEndpoint(host.UTF8String, port.UTF8String);
    NagareNWProxyConfiguration proxy = createProxy(endpoint, NULL);
    if (!proxy) return NO;
    setFailover(proxy, NO);
    void (*addMatchDomain)(NagareNWProxyConfiguration, const char *) = dlsym(network, "nw_proxy_config_add_match_domain");
    if (addMatchDomain) addMatchDomain(proxy, "");
#pragma clang diagnostic push
#pragma clang diagnostic ignored "-Warc-performSelector-leaks"
    [store performSelector:setter withObject:@[(__bridge id)proxy]];
#pragma clang diagnostic pop
    return YES;
}

static BOOL HostMatches(NSString *host, NSArray<NSString *> *patterns) {
    NSString *normalized = host.lowercaseString;
    for (NSString *rawPattern in patterns) {
        NSString *pattern = rawPattern.lowercaseString;
        if ([normalized isEqualToString:pattern]) return YES;
        if ([pattern hasPrefix:@"*."]) {
            NSString *suffix = [pattern substringFromIndex:1];
            NSString *root = [suffix substringFromIndex:1];
            if (![normalized isEqualToString:root] && [normalized hasSuffix:suffix]) return YES;
        }
    }
    return NO;
}

@interface NagareWebViewResolver : NSObject <WKScriptMessageHandler, WKNavigationDelegate, WKUIDelegate>
@property(nonatomic, strong) WKWebView *webView;
@property(nonatomic, strong) WKWebsiteDataStore *dataStore;
@property(nonatomic, strong) NSWindow *window;
@property(nonatomic, strong) NSURL *entryURL;
@property(nonatomic, copy) NSArray<NSString *> *allowedHosts;
@property(nonatomic, copy) NSDictionary<NSString *, NSString *> *headers;
@property(nonatomic, copy) NSString *userAgent;
@property(nonatomic, strong) NSDate *deadline;
@property(nonatomic, strong) NSMutableSet<NSString *> *reportedURLs;
@property(nonatomic) NSInteger maxRedirects;
@property(nonatomic) NSInteger topLevelNavigations;
@property(nonatomic) BOOL finished;
- (instancetype)initWithInput:(NSDictionary *)input;
- (int)run;
@end

@implementation NagareWebViewResolver

+ (NSString *)startScript {
    return @"(() => {"
        "if (window.__nagareInstalled) return; window.__nagareInstalled = true;"
        "const send = (value, evidence) => {"
          "try {"
            "const raw = String(value || '');"
            "const url = new URL(raw, location.href).href;"
            "if (/^https?:/i.test(url) && !url.startsWith('blob:'))"
              "window.webkit.messageHandlers.nagareMedia.postMessage({url, page: location.href, evidence});"
          "} catch (_) {}"
        "};"
        "const originalFetch = window.fetch;"
        "if (originalFetch) window.fetch = async function(...args) {"
          "const response = await originalFetch.apply(this, args);"
          "try {"
            "const type = response.headers.get('content-type') || '';"
            "if (/mpegurl/i.test(type)) send(response.url, 'fetch');"
            "else if (/text|json|octet-stream/i.test(type)) {"
              "const reader = response.clone().body?.getReader();"
              "if (reader) reader.read().then(({value}) => { reader.cancel().catch(() => {}); if (value && new TextDecoder().decode(value.slice(0,4096)).trim().startsWith('#EXTM3U')) send(response.url, 'fetch'); }).catch(() => {});"
            "}"
          "} catch (_) {}"
          "return response;"
        "};"
        "const originalOpen = XMLHttpRequest.prototype.open;"
        "XMLHttpRequest.prototype.open = function(method, url, ...rest) {"
          "this.addEventListener('load', () => {"
            "try {"
              "const type = this.getResponseHeader('content-type') || '';"
              "if (/mpegurl/i.test(type) || (typeof this.responseText === 'string' && this.responseText.trim().startsWith('#EXTM3U')))"
                "send(this.responseURL || url, 'xhr');"
            "} catch (_) {}"
          "});"
          "return originalOpen.call(this, method, url, ...rest);"
        "};"
        "const inspect = () => {"
          "document.querySelectorAll('iframe[loading=lazy]').forEach(frame => frame.removeAttribute('loading'));"
          "document.querySelectorAll('video').forEach(video => {"
            "video.muted = true; video.playsInline = true;"
            "const value = video.currentSrc || video.src || video.getAttribute('src');"
            "if (value) send(value, 'video');"
            "video.querySelectorAll('source').forEach(source => send(source.src || source.getAttribute('src'), 'source'));"
            "if (video.paused) video.play().catch(() => {});"
          "});"
          "performance.getEntriesByType('resource').forEach(entry => {"
            "if (/\\.(m3u8|mp4|m4v|mkv|webm)(?:[?#]|$)/i.test(entry.name)) send(entry.name, 'resource');"
          "});"
        "};"
        "const start = () => {"
          "inspect();"
          "if (document.documentElement) new MutationObserver(inspect).observe(document.documentElement, {childList:true, subtree:true, attributes:true, attributeFilter:['src']});"
          "setInterval(inspect, 400);"
        "};"
        "if (document.readyState === 'loading') document.addEventListener('DOMContentLoaded', start, {once:true}); else start();"
    "})();";
}

- (instancetype)initWithInput:(NSDictionary *)input {
    self = [super init];
    if (!self) return nil;

    NSString *rawURL = [input[@"url"] isKindOfClass:NSString.class] ? input[@"url"] : @"";
    self.entryURL = [NSURL URLWithString:rawURL];
    self.allowedHosts = [input[@"allowedHosts"] isKindOfClass:NSArray.class] ? input[@"allowedHosts"] : @[];
    self.headers = [input[@"headers"] isKindOfClass:NSDictionary.class] ? input[@"headers"] : @{};
    self.maxRedirects = [input[@"maxRedirects"] integerValue];
    NSTimeInterval timeout = MAX(0.1, [input[@"timeoutMs"] doubleValue] / 1000.0);
    self.deadline = [NSDate dateWithTimeIntervalSinceNow:timeout];
    self.reportedURLs = [NSMutableSet set];
    self.userAgent = self.headers[@"User-Agent"] ?: self.headers[@"user-agent"] ?: @"Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/127.0.0.0 Safari/537.36";

    NSDictionary *proxy = [input[@"proxy"] isKindOfClass:NSDictionary.class] ? input[@"proxy"] : @{};
    NSString *proxyHost = [proxy[@"host"] isKindOfClass:NSString.class] ? proxy[@"host"] : @"";
    NSString *proxyPort = [proxy[@"port"] description];
    if (!self.entryURL || self.entryURL.host.length == 0 || self.allowedHosts.count == 0 || proxyHost.length == 0 || proxyPort.length == 0) return nil;

    [NSApplication sharedApplication];
    [NSApp setActivationPolicy:NSApplicationActivationPolicyProhibited];

    WKUserContentController *content = [[WKUserContentController alloc] init];
    [content addScriptMessageHandler:self name:@"nagareMedia"];
    [content addUserScript:[[WKUserScript alloc] initWithSource:NagareWebViewResolver.startScript
                                                 injectionTime:WKUserScriptInjectionTimeAtDocumentStart
                                              forMainFrameOnly:NO]];

    WKWebViewConfiguration *configuration = [[WKWebViewConfiguration alloc] init];
    configuration.userContentController = content;
    self.dataStore = WKWebsiteDataStore.nonPersistentDataStore;
    if (!ConfigureProxy(self.dataStore, proxyHost, proxyPort)) return nil;
    configuration.websiteDataStore = self.dataStore;
    configuration.mediaTypesRequiringUserActionForPlayback = WKAudiovisualMediaTypeNone;

    self.webView = [[WKWebView alloc] initWithFrame:NSMakeRect(0, 0, 640, 360) configuration:configuration];
    self.webView.navigationDelegate = self;
    self.webView.UIDelegate = self;
    self.webView.customUserAgent = self.userAgent;
    self.window = [[NSWindow alloc] initWithContentRect:NSMakeRect(-2000, -2000, 640, 360)
                                               styleMask:NSWindowStyleMaskBorderless
                                                 backing:NSBackingStoreBuffered
                                                   defer:NO];
    self.window.contentView = self.webView;
    self.window.alphaValue = 0.0;
    [self.window orderBack:nil];

    NSArray *cookies = [input[@"cookies"] isKindOfClass:NSArray.class] ? input[@"cookies"] : @[];
    // On macOS 14.5 the Network CONNECT configuration covers HTTPS but does
    // not reliably proxy plain HTTP. Block those page requests before load.
    NSMutableArray *restrictions = [NSMutableArray array];
    for (NSString *scheme in @[@"http", @"ws", @"wss", @"file", @"ftp"]) {
        NSString *filter = [NSString stringWithFormat:@"^%@://",scheme];
        [restrictions addObject:@{@"trigger":@{@"url-filter":filter},@"action":@{@"type":@"block"}}];
    }
    NSString *rules = [[NSString alloc] initWithData:[NSJSONSerialization dataWithJSONObject:restrictions options:0 error:nil] encoding:NSUTF8StringEncoding];
    [WKContentRuleListStore.defaultStore compileContentRuleListForIdentifier:@"nagare-https-only-v1" encodedContentRuleList:rules completionHandler:^(WKContentRuleList *list, NSError *error) {
        if (!list || error) { [self emitError:@"browser_blocked" message:[NSString stringWithFormat:@"native WebView network restrictions could not be installed (%@ %ld: %@)",error.domain,(long)error.code,error.localizedDescription]]; return; }
        [content addContentRuleList:list];
        [self installCookies:cookies completion:^{ [self loadEntry]; }];
    }];
    return self;
}

- (void)installCookies:(NSArray *)values completion:(void (^)(void))completion {
    NSMutableArray<NSHTTPCookie *> *cookies = [NSMutableArray array];
    for (id rawValue in values) {
        if (![rawValue isKindOfClass:NSString.class]) continue;
        NSRange separator = [rawValue rangeOfString:@"="];
        if (separator.location == NSNotFound || separator.location == 0) continue;
        NSString *name = [rawValue substringToIndex:separator.location];
        NSString *value = [rawValue substringFromIndex:separator.location + 1];
        NSMutableDictionary *properties = [@{
            NSHTTPCookieName: name, NSHTTPCookieValue: value,
            NSHTTPCookieDomain: self.entryURL.host, NSHTTPCookiePath: @"/"
        } mutableCopy];
        if ([self.entryURL.scheme.lowercaseString isEqualToString:@"https"]) properties[NSHTTPCookieSecure] = @YES;
        NSHTTPCookie *cookie = [NSHTTPCookie cookieWithProperties:properties];
        if (cookie) [cookies addObject:cookie];
    }
    if (cookies.count == 0) { completion(); return; }
    __block NSInteger remaining = cookies.count;
    for (NSHTTPCookie *cookie in cookies) {
        [self.dataStore.httpCookieStore setCookie:cookie completionHandler:^{
            remaining--;
            if (remaining == 0) completion();
        }];
    }
}

- (void)loadEntry {
    NSMutableURLRequest *request = [NSMutableURLRequest requestWithURL:self.entryURL
                                                          cachePolicy:NSURLRequestReloadIgnoringLocalAndRemoteCacheData
                                                      timeoutInterval:MAX(1, [self.deadline timeIntervalSinceNow])];
    for (NSString *name in self.headers) {
        if ([name caseInsensitiveCompare:@"Cookie"] == NSOrderedSame || [name caseInsensitiveCompare:@"User-Agent"] == NSOrderedSame) continue;
        id value = self.headers[name];
        if ([value isKindOfClass:NSString.class]) [request setValue:value forHTTPHeaderField:name];
    }
    [self.webView loadRequest:request];
}

- (int)run {
    while (!self.finished && self.deadline.timeIntervalSinceNow > 0) {
        [NSRunLoop.currentRunLoop runMode:NSDefaultRunLoopMode beforeDate:[NSDate dateWithTimeIntervalSinceNow:0.05]];
    }
    if (!self.finished) {
        WriteJSON(@{@"event": @"error", @"category": @"resolve_timeout", @"message": @"native WebView did not find media before its deadline"});
        return 2;
    }
    return 0;
}

- (void)emitError:(NSString *)category message:(NSString *)message {
    if (self.finished) return;
    self.finished = YES;
    WriteJSON(@{@"event": @"error", @"category": category ?: @"browser_blocked", @"message": message ?: @"native WebView failed"});
    [self.webView stopLoading];
}

- (void)userContentController:(WKUserContentController *)controller didReceiveScriptMessage:(WKScriptMessage *)message {
    if (self.finished || ![message.body isKindOfClass:NSDictionary.class]) return;
    NSDictionary *body = message.body;
    NSString *rawURL = [body[@"url"] isKindOfClass:NSString.class] ? body[@"url"] : @"";
    NSURL *url = [NSURL URLWithString:rawURL];
    NSString *scheme = url.scheme.lowercaseString;
    if (!url || (![@"http" isEqualToString:scheme] && ![@"https" isEqualToString:scheme]) || [self.reportedURLs containsObject:rawURL] || self.reportedURLs.count >= 64) return;
    [self.reportedURLs addObject:rawURL];

    NSString *page = [body[@"page"] isKindOfClass:NSString.class] ? body[@"page"] : @"";
    NSString *evidence = [body[@"evidence"] isKindOfClass:NSString.class] ? body[@"evidence"] : @"unknown";
    [self.dataStore.httpCookieStore getAllCookies:^(NSArray<NSHTTPCookie *> *cookies) {
        NSMutableArray<NSString *> *cookieValues = [NSMutableArray array];
        for (NSHTTPCookie *cookie in cookies) {
            NSString *domain = cookie.domain.lowercaseString;
            NSString *host = url.host.lowercaseString;
            BOOL matches = [host isEqualToString:domain];
            if ([domain hasPrefix:@"."]) matches = [host isEqualToString:[domain substringFromIndex:1]] || [host hasSuffix:domain];
            NSString *path = url.path.length ? url.path : @"/";
            NSString *cookiePath = cookie.path.length ? cookie.path : @"/";
            BOOL pathMatches = [path isEqualToString:cookiePath] || ([path hasPrefix:cookiePath] && ([cookiePath hasSuffix:@"/"] || [path hasPrefix:[cookiePath stringByAppendingString:@"/"]]));
            if (!matches || !pathMatches || (cookie.secure && ![scheme isEqualToString:@"https"]) || (cookie.expiresDate && cookie.expiresDate.timeIntervalSinceNow <= 0)) continue;
            [cookieValues addObject:[NSString stringWithFormat:@"%@=%@", cookie.name, cookie.value]];
        }
        WriteJSON(@{
            @"event": @"candidate", @"url": rawURL, @"referer": page,
            @"userAgent": self.userAgent, @"cookie": [cookieValues componentsJoinedByString:@"; "],
            @"evidence": evidence
        });
    }];
}

- (void)webView:(WKWebView *)webView decidePolicyForNavigationAction:(WKNavigationAction *)action decisionHandler:(void (^)(WKNavigationActionPolicy))decisionHandler {
    NSURL *url = action.request.URL;
    NSString *scheme = url.scheme.lowercaseString;
    BOOL topLevel = action.targetFrame == nil || action.targetFrame.mainFrame;
    if (!topLevel || [scheme isEqualToString:@"about"]) { decisionHandler(WKNavigationActionPolicyAllow); return; }
    if (![@"https" isEqualToString:scheme] || !HostMatches(url.host ?: @"", self.allowedHosts)) {
        decisionHandler(WKNavigationActionPolicyCancel);
        [self emitError:@"unsafe_redirect" message:@"native WebView top-level navigation crossed the source boundary"];
        return;
    }
    self.topLevelNavigations++;
    if (self.topLevelNavigations > self.maxRedirects + 1) {
        decisionHandler(WKNavigationActionPolicyCancel);
        [self emitError:@"browser_blocked" message:@"native WebView exceeded the navigation limit"];
        return;
    }
    decisionHandler(WKNavigationActionPolicyAllow);
}

- (WKWebView *)webView:(WKWebView *)webView createWebViewWithConfiguration:(WKWebViewConfiguration *)configuration forNavigationAction:(WKNavigationAction *)action windowFeatures:(WKWindowFeatures *)windowFeatures {
    if (action.request.URL) [webView loadRequest:action.request];
    return nil;
}

- (void)webView:(WKWebView *)webView didFailProvisionalNavigation:(WKNavigation *)navigation withError:(NSError *)error {
    [self emitError:@"browser_blocked" message:[NSString stringWithFormat:@"native WebView could not load the playback page (%@ %ld)",error.domain,(long)error.code]];
}

@end

int main(void) {
    @autoreleasepool {
        NSData *inputData = NSFileHandle.fileHandleWithStandardInput.readDataToEndOfFile;
        NSDictionary *input = [NSJSONSerialization JSONObjectWithData:inputData options:0 error:nil];
        if (![input isKindOfClass:NSDictionary.class]) {
            WriteJSON(@{@"event": @"error", @"category": @"browser_blocked", @"message": @"native WebView input is invalid"});
            return 64;
        }
        NagareWebViewResolver *resolver = [[NagareWebViewResolver alloc] initWithInput:input];
        if (!resolver) {
            WriteJSON(@{@"event": @"error", @"category": @"browser_blocked", @"message": @"native WebView or its secure proxy is unavailable; macOS 14 or newer is required"});
            return 69;
        }
        return [resolver run];
    }
}
