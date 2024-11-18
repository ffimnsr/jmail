#!/usr/bin/env python3

import smtplib, ssl

smtp_server = "<ip-of-mail-server>"
smtp_port = 11587
smtp_auth_user = "jmail"
smtp_auth_password = "mysecretpassword"

# Try to log in to server and send email
try:
    server = smtplib.SMTP(smtp_server, smtp_port)
    server.ehlo()
    server.login(smtp_auth_user, smtp_auth_password)
except Exception as e:
    # Print any error messages to stdout
    print(e)
finally:
    server.quit()
