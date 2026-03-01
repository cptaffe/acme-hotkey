#pragma once
#include <CoreGraphics/CoreGraphics.h>
#include <stdint.h>

void startEventTap(void);

/* called from Go to dispose of a buffered original event */
void forwardEvent(uintptr_t eventPtr, unsigned short kc);
void dropEvent(uintptr_t eventPtr, unsigned short kc);

/* defined in frontapp.m */
const char *frontmostProcName(void);

void clearFilter(void);
void addFilterProc(const char *name);
void setListenerCount(int n);
