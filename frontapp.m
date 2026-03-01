#import <AppKit/AppKit.h>
#include <libproc.h>
#include <string.h>

/*
 * frontmostProcName returns the BSD process name (as returned by proc_name(3))
 * of the currently frontmost application, using NSWorkspace.frontmostApplication.
 *
 * This is more reliable than the Accessibility API for apps like devdraw that
 * don't expose a full AX tree — NSWorkspace uses window z-order instead.
 *
 * Safe to call from any thread; NSWorkspace is thread-safe for reads.
 */
const char *
frontmostProcName(void)
{
	static char name[256];
	name[0] = '\0';

	@autoreleasepool {
		NSRunningApplication *app =
		    [[NSWorkspace sharedWorkspace] frontmostApplication];
		if (app)
			proc_name((int)[app processIdentifier],
			    name, (uint32_t)sizeof(name));
	}

	return name;
}
