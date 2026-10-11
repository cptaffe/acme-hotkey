#pragma once
#include <CoreGraphics/CoreGraphics.h>

void        startEventTap(void);          /* run the tap; does not return */
void        initKeyNames(void);           /* build key-name table from current layout */
const char *virtualKeyName(uint16_t kc); /* canonical name for a virtual key code, or NULL */
