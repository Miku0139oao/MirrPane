/*
 * Ported from UxPlay (https://github.com/FDH2/UxPlay), lib/playfair/.
 * Copyright (C) 2012 Juho Vahae-Herttua and contributors.
 *
 * This file is part of a GPLv3 project. UxPlay is licensed under the GNU
 * General Public License v3; see the LICENSE file at the repository root.
 *
 * Changes from upstream: file renamed, this notice prepended, otherwise the
 * logic is unchanged so that it can be checked line by line against UxPlay.
 */
#ifndef PLAYFAIR_H
#define PLAYFAIR_H

void playfair_decrypt(unsigned char* message3, unsigned char* cipherText, unsigned char* keyOut);

#endif
