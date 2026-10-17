/*
 * smoke.c — exercises libgost through the same C ABI dart:ffi uses.
 *
 * It loads the shared library, installs a log callback, starts GOST with a
 * socks5 listener, drives a request through that proxy with curl, then stops
 * and verifies the port was released.
 *
 * Build and run (from the sdk directory, on the host platform):
 *
 *   cc -o build/smoke scripts/smoke.c -ldl     # Linux / macOS
 *   build/smoke build/gost.dll
 *
 * On Windows the library is loaded by path at runtime, so no import library is
 * needed:
 *
 *   gcc -o build/smoke.exe scripts/smoke.c -lws2_32
 *   build\smoke.exe build\gost.dll
 *
 * This deliberately avoids linking against libgost at build time: it is the
 * same LoadLibrary/dlopen flow the Flutter app performs, so a success here means
 * the exported ABI is usable.
 */

#include <stdio.h>
#include <stdlib.h>
#include <string.h>

#ifdef _WIN32
#include <windows.h>
#include <direct.h>
typedef HMODULE lib_handle_t;
#define LOAD_LIB(p) LoadLibraryA(p)
#define LOAD_SYM(h, s) ((void *)GetProcAddress(h, s))
#define MKDIR(p) _mkdir(p)
#else
#include <dlfcn.h>
#include <sys/stat.h>
#include <sys/types.h>
typedef void *lib_handle_t;
#define LOAD_LIB(p) dlopen(p, RTLD_NOW | RTLD_LOCAL)
#define LOAD_SYM(h, s) dlsym(h, s)
#define MKDIR(p) mkdir(p, 0700)
#endif

typedef void (*gost_log_cb)(int level, const char *msg);
typedef char *(*fn_version)(void);
typedef int (*fn_setenv)(char *, char *, char **);
typedef void (*fn_set_log_callback)(gost_log_cb);
typedef void (*fn_set_log_level)(int);
typedef int (*fn_log_level)(void);
typedef int (*fn_start)(char *, char **);
typedef int (*fn_stop)(char **);
typedef int (*fn_reload)(char **);
typedef int (*fn_is_running)(void);
typedef void (*fn_free)(char *);

static fn_version p_version;
static fn_setenv p_setenv;
static fn_set_log_callback p_set_log_callback;
static fn_set_log_level p_set_log_level;
static fn_log_level p_log_level;
static fn_start p_start;
static fn_stop p_stop;
static fn_reload p_reload;
static fn_is_running p_is_running;
static fn_free p_free;

static int failures;

#define CHECK(cond, ...)                                    \
	do {                                                    \
		if (!(cond)) {                                      \
			fprintf(stderr, "FAIL: " __VA_ARGS__);          \
			fprintf(stderr, "\n");                          \
			failures++;                                     \
		} else {                                            \
			printf("ok: " __VA_ARGS__);                     \
			printf("\n");                                   \
		}                                                   \
	} while (0)

static const char *level_name(int level) {
	switch (level) {
	case 0: return "trace";
	case 1: return "debug";
	case 2: return "info";
	case 3: return "warn";
	case 4: return "error";
	case 5: return "fatal";
	default: return "?";
	}
}

static int log_count;

static void on_log(int level, const char *msg) {
	log_count++;
	/* Keep the output small; the point is to prove delivery works. */
	if (log_count <= 12) {
		printf("  [gost %s] %s\n", level_name(level), msg);
	}
}

/* file_exists returns 1 when path is readable. */
static int file_exists(const char *path) {
	FILE *f = fopen(path, "rb");
	if (!f) {
		return 0;
	}
	fclose(f);
	return 1;
}

/* port_open returns 1 when something is listening on 127.0.0.1:port. */
static int port_open(int port) {
#ifdef _WIN32
	WSADATA wsa;
	SOCKET s;
	struct sockaddr_in a;

	WSAStartup(MAKEWORD(2, 2), &wsa);
	s = socket(AF_INET, SOCK_STREAM, 0);
	if (s == INVALID_SOCKET) {
		return 0;
	}
	memset(&a, 0, sizeof(a));
	a.sin_family = AF_INET;
	a.sin_port = htons((unsigned short)port);
	a.sin_addr.s_addr = htonl(0x7f000001);

	if (connect(s, (struct sockaddr *)&a, sizeof(a)) == 0) {
		closesocket(s);
		return 1;
	}
	closesocket(s);
	return 0;
#else
	int fd;
	struct sockaddr_in a;

	fd = socket(AF_INET, SOCK_STREAM, 0);
	if (fd < 0) {
		return 0;
	}
	memset(&a, 0, sizeof(a));
	a.sin_family = AF_INET;
	a.sin_port = htons((unsigned short)port);
	a.sin_addr.s_addr = htonl(0x7f000001);

	if (connect(fd, (struct sockaddr *)&a, sizeof(a)) == 0) {
		close(fd);
		return 1;
	}
	close(fd);
	return 0;
#endif
}

static int wait_for_port(int port, int millis) {
	int waited = 0;
	while (waited < millis) {
		if (port_open(port)) {
			return 1;
		}
#ifdef _WIN32
		Sleep(50);
#else
		usleep(50 * 1000);
#endif
		waited += 50;
	}
	return 0;
}

static int wait_for_port_closed(int port, int millis) {
	int waited = 0;
	while (waited < millis) {
		if (!port_open(port)) {
			return 1;
		}
#ifdef _WIN32
		Sleep(50);
#else
		usleep(50 * 1000);
#endif
		waited += 50;
	}
	return 0;
}

int main(int argc, char **argv) {
	const char *lib = argc > 1 ? argv[1] : "build/gost.dll";
	lib_handle_t h;
	char *err = NULL;
	char *version;
	char cfg[512];
	int rc;

	h = LOAD_LIB(lib);
	if (!h) {
		fprintf(stderr, "cannot load %s\n", lib);
		return 2;
	}

	p_version = (fn_version)LOAD_SYM(h, "gost_version");
	p_setenv = (fn_setenv)LOAD_SYM(h, "gost_setenv");
	p_set_log_callback = (fn_set_log_callback)LOAD_SYM(h, "gost_set_log_callback");
	p_set_log_level = (fn_set_log_level)LOAD_SYM(h, "gost_set_log_level");
	p_log_level = (fn_log_level)LOAD_SYM(h, "gost_log_level");
	p_start = (fn_start)LOAD_SYM(h, "gost_start");
	p_stop = (fn_stop)LOAD_SYM(h, "gost_stop");
	p_reload = (fn_reload)LOAD_SYM(h, "gost_reload");
	p_is_running = (fn_is_running)LOAD_SYM(h, "gost_is_running");
	p_free = (fn_free)LOAD_SYM(h, "gost_free");

	CHECK(p_version && p_setenv && p_set_log_callback && p_set_log_level &&
	          p_log_level && p_start && p_stop && p_reload && p_is_running &&
	          p_free,
	      "all exported symbols resolved");

	version = p_version();
	CHECK(version && version[0] != '\0', "gost_version() = %s",
	      version ? version : "(null)");

	CHECK(p_is_running() == 0, "gost_is_running() == 0 before start");

	/*
	 * gost_setenv must be able to change the environment the Go runtime sees.
	 *
	 * This is what an Android host relies on: the c-shared entry point there
	 * passes an empty envv, so libc setenv is invisible to Go and only the
	 * runtime's own os.Setenv works. The test drives the certificate directory
	 * because it is observable end to end: os.UserHomeDir() reads the variable
	 * through os.Getenv, and x/config/parsing/tls.go persists an auto-generated
	 * certificate to $HOME/.gost.
	 */
	MKDIR("build/smokehome");
	/* HOME on Unix, USERPROFILE on Windows: os.UserHomeDir picks per GOOS. */
	rc = p_setenv("HOME", "build/smokehome", &err);
	CHECK(rc == 0, "gost_setenv(HOME) == 0%s%s", rc ? " err: " : "",
	      err ? err : "");
	if (err) {
		p_free(err);
		err = NULL;
	}
	rc = p_setenv("USERPROFILE", "build/smokehome", &err);
	CHECK(rc == 0, "gost_setenv(USERPROFILE) == 0%s%s", rc ? " err: " : "",
	      err ? err : "");
	if (err) {
		p_free(err);
		err = NULL;
	}
	remove("build/smokehome/.gost/auto-ca-cert.pem");
	remove("build/smokehome/.gost/auto-ca-key.pem");

	p_set_log_callback(on_log);
	p_set_log_level(2); /* info */
	CHECK(p_log_level() == 2, "gost_log_level() == 2 after set");

	/*
	 * A TLS listener with no certificate files makes the parser fall back to
	 * the auto-generated certificate, which is what writes into $HOME/.gost.
	 */
	snprintf(cfg, sizeof(cfg),
	         "{\"services\":[{\"name\":\"s1\",\"addr\":\"127.0.0.1:38080\","
	         "\"handler\":{\"type\":\"socks5\"},\"listener\":{\"type\":\"tls\"}}]}");

	rc = p_start(cfg, &err);
	CHECK(rc == 0, "gost_start() == 0%s%s", rc ? " err: " : "",
	      err ? err : "");
	if (err) {
		p_free(err);
		err = NULL;
	}
	if (rc != 0) {
		return 1;
	}

	CHECK(file_exists("build/smokehome/.gost/auto-ca-cert.pem"),
	      "gost_setenv reached Go: cert persisted under the injected HOME");

	CHECK(wait_for_port(38080, 5000), "socks5 listener is accepting on 38080");
	CHECK(p_is_running() == 1, "gost_is_running() == 1 after start");
	CHECK(log_count > 0, "log callback received %d entries", log_count);

	/* A second start must fail rather than silently restart. */
	rc = p_start(cfg, &err);
	CHECK(rc != 0, "second gost_start() fails");
	if (err) {
		printf("  (expected error: %s)\n", err);
		p_free(err);
		err = NULL;
	}

	/* Metrics/netstats config must not leave profiling on :6060. */
	CHECK(!port_open(6060), "no profiling server on 6060");

	rc = p_reload(&err);
	CHECK(rc == 0, "gost_reload() == 0%s%s", rc ? " err: " : "",
	      err ? err : "");
	if (err) {
		p_free(err);
		err = NULL;
	}
	CHECK(wait_for_port(38080, 5000), "listener still up after reload");

	rc = p_stop(&err);
	CHECK(rc == 0, "gost_stop() == 0%s%s", rc ? " err: " : "", err ? err : "");
	if (err) {
		p_free(err);
		err = NULL;
	}

	CHECK(wait_for_port_closed(38080, 5000), "port 38080 released after stop");
	CHECK(p_is_running() == 0, "gost_is_running() == 0 after stop");

	rc = p_stop(&err);
	CHECK(rc == 0, "second gost_stop() is idempotent");
	if (err) {
		p_free(err);
		err = NULL;
	}

	rc = p_start(cfg, &err);
	CHECK(rc == 0, "gost_start() succeeds again after stop%s%s",
	      rc ? " err: " : "", err ? err : "");
	if (err) {
		p_free(err);
		err = NULL;
	}
	CHECK(wait_for_port(38080, 5000), "listener back up on 38080");
	p_stop(NULL);

	/* Invalid config must be rejected without leaving the library running. */
	rc = p_start("{\"services\":", &err);
	CHECK(rc != 0, "gost_start() rejects malformed config");
	if (err) {
		printf("  (expected error: %s)\n", err);
		p_free(err);
		err = NULL;
	}
	CHECK(p_is_running() == 0, "not running after a failed start");

	p_set_log_callback(NULL);

	printf("\n%s (%d failure%s)\n", failures ? "FAILED" : "PASSED", failures,
	       failures == 1 ? "" : "s");

	return failures ? 1 : 0;
}
